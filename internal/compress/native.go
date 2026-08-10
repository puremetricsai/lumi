package compress

import (
	"context"

	"github.com/puremetricsai/lumi/internal/macosnative"
)

// NativeImages and NativeAudio are the production transcoders: thin adapters
// over internal/macosnative, kept here so this package carries no build tag and
// its tests run anywhere. The stub in internal/macosnative is what makes that
// work — off Apple Silicon these compile and return "unsupported" rather than
// failing to build.

type NativeImages struct{}

func (NativeImages) Transcode(ctx context.Context, source, destination string, quality float64) (macosnative.ImageVerification, error) {
	return macosnative.TranscodeImageHEIC(ctx, source, destination, quality)
}

func (NativeImages) Inspect(ctx context.Context, path string) (macosnative.ImageVerification, error) {
	return macosnative.InspectImage(ctx, path)
}

type NativeAudio struct{}

func (NativeAudio) Transcode(ctx context.Context, source, destination string) error {
	_, err := macosnative.EncodeAudioFLAC(ctx, source, destination)
	return err
}

func (NativeAudio) Decode(ctx context.Context, path string) ([]int16, int, error) {
	return macosnative.DecodeMonoPCM16(ctx, path)
}
