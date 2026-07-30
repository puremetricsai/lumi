package wav

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// lumiLayout builds a file with the exact chunk layout Lumi's own AVAssetWriter
// produces: RIFF/WAVE, a JUNK chunk, fmt with tag 0xFFFE, an FLLR filler chunk,
// and only then data. Verified by parsing real captured files byte by byte.
//
// This is the fixture that matters. A reader that assumed fmt sat at offset 12,
// or that rejected anything but format tag 1, would fail on every chunk Lumi has
// ever recorded while passing a hand-rolled "simple" WAV.
func lumiLayout(samples []int16) []byte {
	fmtChunk := make([]byte, 40)
	binary.LittleEndian.PutUint16(fmtChunk[0:2], formatExtensible)
	binary.LittleEndian.PutUint16(fmtChunk[2:4], 1)      // channels
	binary.LittleEndian.PutUint32(fmtChunk[4:8], 16000)  // sample rate
	binary.LittleEndian.PutUint32(fmtChunk[8:12], 32000) // bytes per second
	binary.LittleEndian.PutUint16(fmtChunk[12:14], 2)    // block align
	binary.LittleEndian.PutUint16(fmtChunk[14:16], 16)   // bits per sample
	binary.LittleEndian.PutUint16(fmtChunk[16:18], 22)   // cbSize
	binary.LittleEndian.PutUint16(fmtChunk[18:20], 16)   // valid bits
	binary.LittleEndian.PutUint32(fmtChunk[20:24], 0)    // channel mask
	copy(fmtChunk[24:40], pcmSubFormatGUID[:])

	data := new(bytes.Buffer)
	for _, sample := range samples {
		binary.Write(data, binary.LittleEndian, sample)
	}

	body := new(bytes.Buffer)
	body.WriteString("WAVE")
	writeChunk(body, "JUNK", make([]byte, 28))
	writeChunk(body, "fmt ", fmtChunk)
	writeChunk(body, "FLLR", make([]byte, 16))
	writeChunk(body, "data", data.Bytes())

	out := new(bytes.Buffer)
	out.WriteString("RIFF")
	binary.Write(out, binary.LittleEndian, uint32(body.Len()))
	out.Write(body.Bytes())
	return out.Bytes()
}

func writeChunk(w *bytes.Buffer, id string, body []byte) {
	w.WriteString(id)
	binary.Write(w, binary.LittleEndian, uint32(len(body)))
	w.Write(body)
	if len(body)%2 == 1 {
		w.WriteByte(0)
	}
}

func tone(count int, amplitude int16) []int16 {
	samples := make([]int16, count)
	for i := range samples {
		samples[i] = int16(float64(amplitude) * math.Sin(2*math.Pi*440*float64(i)/16000))
	}
	return samples
}

func TestDecodesLumiWriterLayout(t *testing.T) {
	want := tone(16000, 8000)
	samples, info, err := DecodeMono16(lumiLayout(want))
	if err != nil {
		t.Fatal(err)
	}
	if info.SampleRate != 16000 || info.Channels != 1 || info.BitsPerSample != 16 {
		t.Errorf("info = %+v", info)
	}
	if len(samples) != len(want) {
		t.Fatalf("decoded %d samples, want %d", len(samples), len(want))
	}
	for i := range want {
		if samples[i] != want[i] {
			t.Fatalf("sample %d = %d, want %d", i, samples[i], want[i])
		}
	}
	if info.Truncated {
		t.Error("a complete file was reported truncated")
	}
	if got := info.Duration(); got != 1000 {
		t.Errorf("duration = %dms, want 1000", got)
	}
}

func TestDecodesPlainPCMTag(t *testing.T) {
	file := lumiLayout(tone(1600, 4000))
	// Rewrite the format tag to plain PCM in place. The GUID stays behind it and
	// must simply be ignored.
	index := bytes.Index(file, []byte("fmt "))
	if index < 0 {
		t.Fatal("fixture lost its fmt chunk")
	}
	binary.LittleEndian.PutUint16(file[index+8:index+10], formatPCM)
	if _, _, err := DecodeMono16(file); err != nil {
		t.Fatalf("plain PCM tag rejected: %v", err)
	}
}

func TestRejectsUnsupportedFormats(t *testing.T) {
	t.Run("float tag", func(t *testing.T) {
		file := lumiLayout(tone(160, 1000))
		index := bytes.Index(file, []byte("fmt "))
		binary.LittleEndian.PutUint16(file[index+8:index+10], 0x0003) // IEEE float
		if _, _, err := DecodeMono16(file); err == nil {
			t.Error("accepted a float-format file")
		}
	})
	t.Run("non-PCM subformat", func(t *testing.T) {
		file := lumiLayout(tone(160, 1000))
		index := bytes.Index(file, []byte("fmt "))
		file[index+8+24] ^= 0xFF // corrupt the SubFormat GUID
		if _, _, err := DecodeMono16(file); err == nil {
			t.Error("accepted an extensible file whose SubFormat is not PCM")
		}
	})
	t.Run("stereo", func(t *testing.T) {
		file := lumiLayout(tone(160, 1000))
		index := bytes.Index(file, []byte("fmt "))
		binary.LittleEndian.PutUint16(file[index+8+2:index+8+4], 2)
		if _, _, err := DecodeMono16(file); err == nil {
			t.Error("accepted a stereo file")
		}
	})
	t.Run("not a riff file", func(t *testing.T) {
		if _, _, err := DecodeMono16([]byte("this is not audio at all")); err == nil {
			t.Error("accepted a non-RIFF file")
		}
	})
}

// TestTruncatedDataChunkIsDecodedNotRefused pins the "never lose captured media"
// choice: a chunk cut short by a crash still holds usable audio, so it is decoded
// up to what exists and flagged, rather than refused over a header mismatch.
func TestTruncatedDataChunkIsDecodedNotRefused(t *testing.T) {
	file := lumiLayout(tone(1600, 4000))
	short := file[:len(file)-1000]
	samples, info, err := DecodeMono16(short)
	if err != nil {
		t.Fatalf("truncated file refused: %v", err)
	}
	if !info.Truncated {
		t.Error("truncation was not reported")
	}
	if len(samples) != 1600-500 {
		t.Errorf("decoded %d samples, want %d", len(samples), 1600-500)
	}
}

// TestDigitalSilenceAndSignalAreFarApart is the measurement the origin gate rests
// on. Real captures showed a system track holding exactly zero samples while the
// user spoke, against -46 dBFS for one holding untranscribed machine audio. The
// gate only has to find that gap, not judge loudness.
func TestDigitalSilenceAndSignalAreFarApart(t *testing.T) {
	silent := RMSDBFS(make([]int16, 16000))
	if silent != SilenceFloorDBFS {
		t.Errorf("digital silence = %v dBFS, want the floor %v", silent, SilenceFloorDBFS)
	}
	// Roughly -46 dBFS, the level real untranscribed machine audio measured.
	quiet := RMSDBFS(tone(16000, 232))
	if quiet <= -60 || quiet >= -35 {
		t.Errorf("quiet signal = %.1f dBFS, expected roughly -46", quiet)
	}
	if quiet-silent < 50 {
		t.Errorf("only %.1f dB separates silence from a quiet signal", quiet-silent)
	}
	if got := RMSDBFS(nil); got != SilenceFloorDBFS {
		t.Errorf("empty input = %v, want the floor", got)
	}
}

// TestEnvelopeIsLocalNotWholeFile is why Envelope exists. One short blip in an
// otherwise silent track must leave the rest of the track readable as silent —
// a whole-file RMS would raise every window at once and mark thirty seconds of
// the user's speech ambiguous.
func TestEnvelopeIsLocalNotWholeFile(t *testing.T) {
	samples := make([]int16, 16000) // one second of silence
	copy(samples[8000:8800], tone(800, 12000))

	envelope := Envelope(samples, 16000, 100)
	if len(envelope) != 10 {
		t.Fatalf("got %d windows, want 10", len(envelope))
	}
	loud := 0
	for i, level := range envelope {
		if level > -40 {
			loud++
			if i != 5 && i != 6 {
				t.Errorf("window %d is loud at %.1f dBFS but the blip was at 500..550ms", i, level)
			}
		}
	}
	if loud == 0 {
		t.Fatal("the blip did not register in any window")
	}
	if envelope[0] != SilenceFloorDBFS || envelope[9] != SilenceFloorDBFS {
		t.Errorf("silent windows read %.1f and %.1f dBFS, want the floor",
			envelope[0], envelope[9])
	}
	if whole := RMSDBFS(samples); whole <= SilenceFloorDBFS {
		t.Fatal("fixture is not actually loud overall; the test proves nothing")
	}
}

func TestEnvelopeIncludesThePartialTail(t *testing.T) {
	// 250ms at a 100ms window: two whole windows and a 50ms remainder that must
	// not be silently dropped.
	envelope := Envelope(tone(4000, 6000), 16000, 100)
	if len(envelope) != 3 {
		t.Fatalf("got %d windows, want 3 including the partial tail", len(envelope))
	}
	for i, level := range envelope {
		if level <= -40 {
			t.Errorf("window %d reads %.1f dBFS for a continuous tone", i, level)
		}
	}
}

// TestReadEnvelopeMatchesDecodingFirst pins the byte-level shortcut against the
// readable implementation it exists to avoid.
//
// ReadEnvelope skips materializing the stream, which is worth ~960KB per chunk on
// the recorder's per-chunk path — but only while it produces exactly what
// Envelope would. Without this, the two could drift on window boundaries or the
// partial tail and every energy verdict would quietly shift, with Envelope's own
// tests still passing because nothing in production calls it any more.
func TestReadEnvelopeMatchesDecodingFirst(t *testing.T) {
	samples := make([]int16, 16000)
	copy(samples[8000:8800], tone(800, 12000))
	// A trailing sample count that is not a multiple of the window, so the
	// partial tail is exercised too.
	samples = append(samples, tone(750, 9000)...)

	path := filepath.Join(t.TempDir(), "chunk.wav")
	if err := os.WriteFile(path, lumiLayout(samples), 0o600); err != nil {
		t.Fatal(err)
	}

	direct, info, err := ReadEnvelope(path, 100)
	if err != nil {
		t.Fatalf("ReadEnvelope: %v", err)
	}
	if info.Samples != len(samples) {
		t.Errorf("info reports %d samples, want %d", info.Samples, len(samples))
	}
	want := Envelope(samples, info.SampleRate, 100)
	if len(direct) != len(want) {
		t.Fatalf("got %d windows, want %d", len(direct), len(want))
	}
	for i := range want {
		if direct[i] != want[i] {
			t.Errorf("window %d: %.6f dBFS, want %.6f", i, direct[i], want[i])
		}
	}
}

// TestReadsRealCapturedFile is the only check that the synthesized fixture above
// actually matches reality. Real captures live outside the repository, so point
// it at one:
//
//	LUMI_WAV_FILE="$HOME/Library/Application Support/Lumi/audio/…-system.wav" \
//	  go test ./internal/wav -run RealCaptured -v
func TestReadsRealCapturedFile(t *testing.T) {
	path := os.Getenv("LUMI_WAV_FILE")
	if path == "" {
		t.Skip("set LUMI_WAV_FILE to a captured WAV")
	}
	samples, info, err := ReadMono16(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.SampleRate != 16000 || info.Channels != 1 || info.BitsPerSample != 16 {
		t.Errorf("unexpected format for a Lumi capture: %+v", info)
	}
	envelope := Envelope(samples, info.SampleRate, 100)
	active := 0
	for _, level := range envelope {
		if level > -60 {
			active++
		}
	}
	t.Logf("%s: %d samples (%dms), %.1f dBFS overall, %d/%d windows above -60 dBFS",
		filepath.Base(path), info.Samples, info.Duration(), RMSDBFS(samples), active, len(envelope))
	if info.Samples == 0 {
		t.Error("decoded no samples from a real capture")
	}
}

func TestEnvelopeHandlesDegenerateInput(t *testing.T) {
	if got := Envelope(nil, 16000, 100); got != nil {
		t.Errorf("empty samples returned %v", got)
	}
	if got := Envelope(tone(160, 1000), 0, 100); got != nil {
		t.Errorf("zero sample rate returned %v", got)
	}
	if got := Envelope(tone(160, 1000), 16000, 0); got != nil {
		t.Errorf("zero window returned %v", got)
	}
}
