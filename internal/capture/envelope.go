package capture

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/puremetricsai/lumi/internal/macosnative"
	"github.com/puremetricsai/lumi/internal/seal"
	"github.com/puremetricsai/lumi/internal/wav"
)

// nativeDecodePCM16 is a package var only so the non-WAV branch is reachable in
// a test. Without it that branch would need a Mac and a real FLAC to exercise,
// so the dispatch rule — the whole reason this file exists — would go untested.
var nativeDecodePCM16 = macosnative.DecodeMonoPCM16

// ReadAudioEnvelope reduces one captured audio file to a windowed energy
// envelope, whatever container it is stored in.
//
// It exists as one function because the recorder and the backfill both ask this
// question, and a rule copied across that boundary is correct only until one
// copy moves — invisibly to both test suites, since neither writer can see the
// other. The energy gate has already been the cautionary case here once.
//
// The dispatch is needed because `lumi compress` rewrites a chunk's WAV as FLAC
// and internal/wav reads mono 16-bit PCM RIFF only, deliberately: it is pure Go,
// so it builds and tests anywhere, and pulling AVFoundation into it would make a
// package that needs no Mac need one. So the split is by half rather than by
// package — internal/macosnative knows about containers, internal/wav measures
// samples, and this chooses between them on the extension.
//
// Getting this wrong is quiet rather than loud, which is why it is centralised:
// both callers discard the error, so a chunk whose envelope could not be read
// simply reaches its bleed verdict without one, with no diagnostic anywhere.
// It is also the one place that knows a stored audio file may be sealed. The
// recorder measures a chunk it has just written, which is still plaintext; the
// backfill measures chunks from disk, which are not. Both arrive here, so the
// unseal happens once — and because seal.Key's zero value is a pass-through and
// its readers accept plaintext either way, neither caller branches on it.
func ReadAudioEnvelope(ctx context.Context, key seal.Key, path string, windowMS int) ([]float64, wav.Info, error) {
	// Matched on ".wav" rather than on ".flac", so a container Lumi stores later
	// takes the native path automatically instead of failing as a RIFF file.
	// Sealing never changes an extension, so this dispatch is unaffected by it.
	if strings.EqualFold(filepath.Ext(path), ".wav") {
		raw, err := key.ReadFile(path)
		if err != nil {
			return nil, wav.Info{}, err
		}
		return wav.EnvelopeFromBytes(raw, windowMS)
	}
	// The native decoder takes a path, so a sealed file needs a plaintext copy
	// for the length of the call. TempCopy hands back the original path when
	// there is nothing to unseal, so this costs nothing when encryption is off.
	decodable, cleanup, err := key.TempCopy(path)
	if err != nil {
		return nil, wav.Info{}, fmt.Errorf("decrypt %s for energy measurement: %w", filepath.Ext(path), err)
	}
	defer cleanup()
	samples, sampleRate, err := nativeDecodePCM16(ctx, decodable)
	if err != nil {
		return nil, wav.Info{}, fmt.Errorf("decode %s for energy measurement: %w", filepath.Ext(path), err)
	}
	// The native decoder is asked for mono 16-bit samples, so this describes what
	// it returned rather than re-deriving anything.
	//
	// Truncated stays false, which is a narrower claim than it looks: the native
	// decoder stops at a short read and reports what it got, so a truncated
	// compressed file decodes short without saying so. Nothing reads Truncated on
	// this path — both callers discard Info and want only the envelope — so this
	// is a gap in what the field could mean rather than a wrong answer given to
	// anybody. Filling it in means reporting the shortfall from the bridge.
	info := wav.Info{
		SampleRate:    sampleRate,
		Channels:      1,
		BitsPerSample: 16,
		Samples:       len(samples),
	}
	return wav.Envelope(samples, sampleRate, windowMS), info, nil
}
