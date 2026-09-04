package capture

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/puremetricsai/lumi/internal/transcript"
	"github.com/puremetricsai/lumi/internal/wav"
)

// writeEnvelopeWAV writes the format Lumi captures — 16 kHz mono 16-bit
// little-endian PCM — so the pure-Go reader accepts it.
func writeEnvelopeWAV(t *testing.T, path string, samples []int16) {
	t.Helper()
	body := make([]byte, len(samples)*2)
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(body[i*2:], uint16(sample))
	}
	var buffer bytes.Buffer
	buffer.WriteString("RIFF")
	binary.Write(&buffer, binary.LittleEndian, uint32(36+len(body)))
	buffer.WriteString("WAVEfmt ")
	binary.Write(&buffer, binary.LittleEndian, uint32(16))
	binary.Write(&buffer, binary.LittleEndian, uint16(1))
	binary.Write(&buffer, binary.LittleEndian, uint16(1))
	binary.Write(&buffer, binary.LittleEndian, uint32(16000))
	binary.Write(&buffer, binary.LittleEndian, uint32(32000))
	binary.Write(&buffer, binary.LittleEndian, uint16(2))
	binary.Write(&buffer, binary.LittleEndian, uint16(16))
	buffer.WriteString("data")
	binary.Write(&buffer, binary.LittleEndian, uint32(len(body)))
	buffer.Write(body)
	if err := os.WriteFile(path, buffer.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func envelopeSamples() []int16 {
	samples := make([]int16, 16000)
	for i := range samples[:len(samples)/2] {
		samples[i] = int16(9000 * math.Sin(2*math.Pi*440*float64(i)/16000))
	}
	return samples
}

// stubDecoder replaces the native decoder so the compressed branch is reachable
// without a Mac, a FLAC file, or a permission grant.
func stubDecoder(t *testing.T, samples []int16, sampleRate int, err error) *string {
	t.Helper()
	var decoded string
	original := nativeDecodePCM16
	nativeDecodePCM16 = func(_ context.Context, path string) ([]int16, int, error) {
		decoded = path
		return samples, sampleRate, err
	}
	t.Cleanup(func() { nativeDecodePCM16 = original })
	return &decoded
}

func TestReadAudioEnvelopeReadsWAVsWithoutTheNativeDecoder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chunk.wav")
	samples := envelopeSamples()
	writeEnvelopeWAV(t, path, samples)
	// A native decoder that fails loudly, so a WAV routed through it fails the
	// test rather than quietly producing the same numbers by luck.
	decoded := stubDecoder(t, nil, 0, errors.New("the native decoder must not be used for a WAV"))

	envelope, info, err := ReadAudioEnvelope(context.Background(), nil, path, transcript.EnvelopeWindowMS)
	if err != nil {
		t.Fatal(err)
	}
	if *decoded != "" {
		t.Errorf("a WAV was sent to the native decoder (%s)", *decoded)
	}
	if info.SampleRate != 16000 || info.Channels != 1 || info.BitsPerSample != 16 {
		t.Errorf("described the WAV as %d Hz, %d channels, %d-bit", info.SampleRate, info.Channels, info.BitsPerSample)
	}
	want := wav.Envelope(samples, 16000, transcript.EnvelopeWindowMS)
	if len(envelope) != len(want) {
		t.Fatalf("envelope has %d windows, want %d", len(envelope), len(want))
	}
}

func TestReadAudioEnvelopeDecodesCompressedAudioNatively(t *testing.T) {
	// The same samples through both branches must produce the same envelope, or
	// `lumi compress` changes a chunk's bleed verdict by changing its container.
	samples := envelopeSamples()
	wavPath := filepath.Join(t.TempDir(), "chunk.wav")
	writeEnvelopeWAV(t, wavPath, samples)
	fromWAV, _, err := ReadAudioEnvelope(context.Background(), nil, wavPath, transcript.EnvelopeWindowMS)
	if err != nil {
		t.Fatal(err)
	}

	flacPath := filepath.Join(t.TempDir(), "chunk.flac")
	if err := os.WriteFile(flacPath, []byte("pretend FLAC"), 0o600); err != nil {
		t.Fatal(err)
	}
	decoded := stubDecoder(t, samples, 16000, nil)

	fromFLAC, info, err := ReadAudioEnvelope(context.Background(), nil, flacPath, transcript.EnvelopeWindowMS)
	if err != nil {
		t.Fatal(err)
	}
	if *decoded != flacPath {
		t.Errorf("native decoder read %q, want %q", *decoded, flacPath)
	}
	if info.SampleRate != 16000 || info.Samples != len(samples) {
		t.Errorf("described the decoded audio as %d Hz, %d samples", info.SampleRate, info.Samples)
	}
	if len(fromFLAC) != len(fromWAV) {
		t.Fatalf("compressed audio produced %d windows, the same audio as a WAV produced %d",
			len(fromFLAC), len(fromWAV))
	}
	for i := range fromWAV {
		if fromFLAC[i] != fromWAV[i] {
			t.Fatalf("window %d differs by container: %v from FLAC, %v from WAV", i, fromFLAC[i], fromWAV[i])
		}
	}
}

// Anything that is not a WAV takes the native path, so a container Lumi stores
// later needs no change here. Matching on ".flac" instead would send it to the
// RIFF reader, which would reject it.
func TestReadAudioEnvelopeSendsAnyNonWAVContainerToTheDecoder(t *testing.T) {
	for _, name := range []string{"chunk.flac", "chunk.m4a", "chunk.caf", "chunk"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(path, []byte("audio"), 0o600); err != nil {
				t.Fatal(err)
			}
			decoded := stubDecoder(t, envelopeSamples(), 16000, nil)
			if _, _, err := ReadAudioEnvelope(context.Background(), nil, path, transcript.EnvelopeWindowMS); err != nil {
				t.Fatal(err)
			}
			if *decoded != path {
				t.Errorf("%s did not reach the native decoder", name)
			}
		})
	}
}

func TestReadAudioEnvelopeMatchesTheExtensionCaseInsensitively(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chunk.WAV")
	writeEnvelopeWAV(t, path, envelopeSamples())
	decoded := stubDecoder(t, nil, 0, errors.New("the native decoder must not be used for a WAV"))
	if _, _, err := ReadAudioEnvelope(context.Background(), nil, path, transcript.EnvelopeWindowMS); err != nil {
		t.Fatal(err)
	}
	if *decoded != "" {
		t.Error("an upper-case .WAV was sent to the native decoder")
	}
}

func TestReadAudioEnvelopeReportsADecodeFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chunk.flac")
	if err := os.WriteFile(path, []byte("not audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	stubDecoder(t, nil, 0, errors.New("unsupported file type"))
	if _, _, err := ReadAudioEnvelope(context.Background(), nil, path, transcript.EnvelopeWindowMS); err == nil {
		t.Error("a failed decode was reported as a successful measurement")
	}
}
