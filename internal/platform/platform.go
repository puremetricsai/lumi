package platform

import (
	"fmt"
	"runtime"

	"github.com/puremetricsai/lumi/internal/macosnative"
)

func Validate() error {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return fmt.Errorf("lumi v1 requires Apple Silicon macOS (running %s/%s)", runtime.GOOS, runtime.GOARCH)
	}
	major, minor, patch, err := macosnative.OSVersion()
	if err != nil {
		return fmt.Errorf("inspect macOS version: %w", err)
	}
	if major < 26 {
		return fmt.Errorf("lumi native capture requires macOS 26 or newer (running %d.%d.%d)", major, minor, patch)
	}
	return nil
}
