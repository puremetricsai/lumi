package platform

import "testing"

func TestValidateAcceptsMacOS26Host(t *testing.T) {
	// This test runs on the dev/CI host, which must be macOS 26+ for Lumi.
	if err := Validate(); err != nil {
		t.Fatalf("Validate rejected a supported host: %v", err)
	}
}
