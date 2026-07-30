// Package wav reads the mono 16-bit PCM WAV files Lumi captures and measures
// their energy.
//
// It exists for one reason that is not obvious: the recognizer cannot tell a
// silent audio track from one that carried sound but produced no words. Both
// return zero results. Measured on captured chunks, a system track recorded
// while the user spoke for thirty seconds holds *exactly zero samples*, while
// one holding untranscribed machine audio measured -46 dBFS. Only energy
// separates those two cases, and they need opposite conclusions: silence means
// nothing was playing, so microphone speech is confidently the room's; energy
// without words means transcription failed, so its origin is unknown.
//
// The package is pure Go with no cgo, so every rule here is testable without
// permissions or the native speech library.
package wav

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
)

// SilenceFloorDBFS is the value reported for a window of pure digital silence.
// Real dBFS would be negative infinity, which propagates through comparisons and
// averages badly, so it is clamped here instead.
const SilenceFloorDBFS = -120.0

const (
	formatPCM        = 0x0001
	formatExtensible = 0xFFFE
)

// pcmSubFormatGUID is KSDATAFORMAT_SUBTYPE_PCM, the SubFormat an extensible
// header must declare for its samples to be plain PCM. Lumi's own AVAssetWriter
// emits exactly this.
var pcmSubFormatGUID = [16]byte{
	0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00,
	0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71,
}

// Info describes the decoded stream.
type Info struct {
	SampleRate    int
	Channels      int
	BitsPerSample int
	// Samples is the number of frames decoded, which for mono is the sample
	// count. It can be fewer than the data chunk's declared size implies when a
	// capture was cut short; see ReadMono16.
	Samples int
	// Truncated reports that the data chunk declared more bytes than the file
	// actually holds.
	Truncated bool
}

// Duration returns the stream's length in milliseconds.
func (i Info) Duration() int64 {
	if i.SampleRate <= 0 {
		return 0
	}
	return int64(i.Samples) * 1000 / int64(i.SampleRate)
}

// ReadMono16 decodes a mono 16-bit PCM WAV file.
//
// Chunks are walked generically rather than assumed to sit at fixed offsets, and
// that is a requirement rather than defensiveness: Lumi's writer emits
// RIFF/WAVE, then a JUNK chunk, then fmt with tag 0xFFFE
// (WAVE_FORMAT_EXTENSIBLE), then an FLLR filler chunk, and only then data. A
// reader assuming fmt at offset 12, or rejecting anything but tag 1, fails on
// every file Lumi has ever recorded — Python's standard wave module rejects them
// outright for exactly that reason.
//
// A data chunk declaring more bytes than the file holds is decoded up to what is
// there and flagged in Info.Truncated rather than refused. A chunk cut short by
// a crash still carries usable audio, and refusing it would lose captured media
// to a header inconsistency.
func ReadMono16(path string) ([]int16, Info, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, Info{}, err
	}
	return DecodeMono16(raw)
}

// ReadEnvelope reads a WAV and reduces it straight to a windowed energy envelope.
//
// It exists because that is the only thing either caller of this package wants:
// both read a chunk's system track, hand the samples to Envelope, and discard
// them. Going through ReadMono16 would allocate a whole 30-second stream — 960 KB
// per chunk, on the recorder's per-chunk path and on every chunk a backfill walks
// — purely to sum it window by window. Info comes back for the same reasons
// ReadMono16 returns it, minus the samples nobody kept.
func ReadEnvelope(path string, windowMS int) ([]float64, Info, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, Info{}, err
	}
	data, info, err := decodeMono16(raw)
	if err != nil {
		return nil, Info{}, err
	}
	return envelopeBytes(data, info.SampleRate, windowMS), info, nil
}

// DecodeMono16 is ReadMono16 over an in-memory file, so tests need no fixtures on
// disk.
func DecodeMono16(raw []byte) ([]int16, Info, error) {
	data, info, err := decodeMono16(raw)
	if err != nil {
		return nil, Info{}, err
	}
	samples := make([]int16, len(data)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(data[i*2 : i*2+2]))
	}
	return samples, info, nil
}

// decodeMono16 validates the header and returns the data chunk's bytes, trimmed
// to whole samples. It is split out so an envelope can be taken without
// materializing the stream first.
func decodeMono16(raw []byte) ([]byte, Info, error) {
	if len(raw) < 12 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		return nil, Info{}, errors.New("not a RIFF/WAVE file")
	}

	var (
		info  Info
		data  []byte
		haveF bool
	)
	for offset := 12; offset+8 <= len(raw); {
		id := string(raw[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(raw[offset+4 : offset+8]))
		body := offset + 8
		if size < 0 {
			return nil, Info{}, fmt.Errorf("chunk %q declares a negative size", id)
		}
		end := body + size
		truncated := false
		if end > len(raw) {
			end = len(raw)
			truncated = true
		}

		switch id {
		case "fmt ":
			parsed, err := parseFormat(raw[body:end])
			if err != nil {
				return nil, Info{}, err
			}
			info.SampleRate = parsed.sampleRate
			info.Channels = parsed.channels
			info.BitsPerSample = parsed.bits
			haveF = true
		case "data":
			data = raw[body:end]
			info.Truncated = truncated
		}

		// Chunk bodies are padded to an even length; the pad byte is not counted
		// in the declared size.
		advance := 8 + size
		if size%2 == 1 {
			advance++
		}
		if advance <= 0 {
			return nil, Info{}, fmt.Errorf("chunk %q would not advance the reader", id)
		}
		offset += advance
	}

	if !haveF {
		return nil, Info{}, errors.New("no fmt chunk")
	}
	if data == nil {
		return nil, Info{}, errors.New("no data chunk")
	}
	if info.Channels != 1 {
		return nil, Info{}, fmt.Errorf("expected mono audio, got %d channels", info.Channels)
	}
	if info.BitsPerSample != 16 {
		return nil, Info{}, fmt.Errorf("expected 16-bit samples, got %d-bit", info.BitsPerSample)
	}

	// A trailing odd byte cannot form a sample; drop it rather than misalign
	// every sample after it.
	data = data[:len(data)-len(data)%2]
	info.Samples = len(data) / 2
	return data, info, nil
}

type format struct {
	sampleRate int
	channels   int
	bits       int
}

func parseFormat(body []byte) (format, error) {
	if len(body) < 16 {
		return format{}, fmt.Errorf("fmt chunk is %d bytes, too short to describe a stream", len(body))
	}
	tag := binary.LittleEndian.Uint16(body[0:2])
	parsed := format{
		channels:   int(binary.LittleEndian.Uint16(body[2:4])),
		sampleRate: int(binary.LittleEndian.Uint32(body[4:8])),
		bits:       int(binary.LittleEndian.Uint16(body[14:16])),
	}

	switch tag {
	case formatPCM:
		return parsed, nil
	case formatExtensible:
		// The extension is cbSize(2) validBits(2) channelMask(4) SubFormat(16),
		// so the SubFormat GUID begins 24 bytes into the chunk.
		if len(body) < 40 {
			return format{}, errors.New("extensible fmt chunk is missing its SubFormat GUID")
		}
		var guid [16]byte
		copy(guid[:], body[24:40])
		if guid != pcmSubFormatGUID {
			return format{}, errors.New("extensible fmt chunk declares a SubFormat that is not PCM")
		}
		return parsed, nil
	default:
		return format{}, fmt.Errorf("unsupported WAV format tag 0x%04X", tag)
	}
}

// RMSDBFS reports the whole stream's root-mean-square level in dBFS, clamped at
// SilenceFloorDBFS.
//
// It is a *presence* measure and must never be used as a speech detector.
// Capture gain varies enough between sessions that a "loud enough to be speech"
// threshold is not portable: clear speech measured -26 dBFS in one recording and
// -68 dBFS in another. What is portable is the distance between digital silence
// and any real signal at all.
func RMSDBFS(samples []int16) float64 {
	if len(samples) == 0 {
		return SilenceFloorDBFS
	}
	var sum float64
	for _, sample := range samples {
		value := float64(sample)
		sum += value * value
	}
	return dbfs(math.Sqrt(sum / float64(len(samples))))
}

// Envelope reports the level in dBFS of each fixed-length window of the stream,
// in order. The final partial window is included so the tail of a stream is
// never invisible.
//
// A windowed envelope rather than one whole-file measurement is the point:
// a single notification blip lifts a thirty-second RMS above any threshold, which
// would mark half a minute of the user's speech as ambiguous. Asking "was the
// other track quiet *here*" needs local energy.
func Envelope(samples []int16, sampleRate, windowMS int) []float64 {
	if len(samples) == 0 || sampleRate <= 0 || windowMS <= 0 {
		return nil
	}
	window := sampleRate * windowMS / 1000
	if window <= 0 {
		window = 1
	}
	count := (len(samples) + window - 1) / window
	envelope := make([]float64, 0, count)
	for start := 0; start < len(samples); start += window {
		end := start + window
		if end > len(samples) {
			end = len(samples)
		}
		envelope = append(envelope, RMSDBFS(samples[start:end]))
	}
	return envelope
}

// envelopeBytes is Envelope over undecoded little-endian samples, so a file read
// only for its energy never has to become an []int16. It produces exactly what
// Envelope would for the same stream.
func envelopeBytes(data []byte, sampleRate, windowMS int) []float64 {
	count := len(data) / 2
	if count == 0 || sampleRate <= 0 || windowMS <= 0 {
		return nil
	}
	window := sampleRate * windowMS / 1000
	if window <= 0 {
		window = 1
	}
	envelope := make([]float64, 0, (count+window-1)/window)
	for start := 0; start < count; start += window {
		end := start + window
		if end > count {
			end = count
		}
		var sum float64
		for i := start; i < end; i++ {
			value := float64(int16(binary.LittleEndian.Uint16(data[i*2 : i*2+2])))
			sum += value * value
		}
		envelope = append(envelope, dbfs(math.Sqrt(sum/float64(end-start))))
	}
	return envelope
}

func dbfs(rms float64) float64 {
	if rms <= 0 {
		return SilenceFloorDBFS
	}
	value := 20 * math.Log10(rms/32768.0)
	if value < SilenceFloorDBFS || math.IsNaN(value) {
		return SilenceFloorDBFS
	}
	return value
}
