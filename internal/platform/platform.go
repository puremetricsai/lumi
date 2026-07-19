package platform

import (
	"fmt"
	"runtime"
)

func Validate() error {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return fmt.Errorf("lumi v1 requires Apple Silicon macOS (running %s/%s)", runtime.GOOS, runtime.GOARCH)
	}
	return nil
}
