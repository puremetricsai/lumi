//go:build darwin && arm64 && cgo

package macosnative

import (
	"bytes"
	"errors"
	"os"
	"testing"
)

// TestEncryptionKeyRoundTrip touches the real login Keychain, so it runs only
// when LUMI_KEYCHAIN_TEST is set. It is the same rule the rest of this package
// applies to permission-gated tests: `task test` must never prompt, and must
// never leave an item behind on a developer's machine.
func TestEncryptionKeyRoundTrip(t *testing.T) {
	if os.Getenv("LUMI_KEYCHAIN_TEST") == "" {
		t.Skip("set LUMI_KEYCHAIN_TEST=1 to exercise the real Keychain")
	}
	t.Cleanup(func() {
		if err := DeleteEncryptionKey(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	if err := DeleteEncryptionKey(); err != nil {
		t.Fatalf("deleting a key that is not there must succeed: %v", err)
	}
	if has, err := HasEncryptionKey(); err != nil || has {
		t.Fatalf("HasEncryptionKey = %v, %v; want false, nil", has, err)
	}
	if _, err := EncryptionKey(); !errors.Is(err, ErrNoEncryptionKey) {
		t.Fatalf("EncryptionKey with nothing stored = %v, want ErrNoEncryptionKey", err)
	}

	want := bytes.Repeat([]byte{0xA7}, EncryptionKeyLen)
	if err := StoreEncryptionKey(want); err != nil {
		t.Fatal(err)
	}
	if has, err := HasEncryptionKey(); err != nil || !has {
		t.Fatalf("HasEncryptionKey after storing = %v, %v; want true, nil", has, err)
	}
	got, err := EncryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("the key read back is not the key stored")
	}
	// Storing twice must replace rather than fail: a rebuild changes this
	// binary's code identity, and re-adding is how the ACL is re-established.
	if err := StoreEncryptionKey(want); err != nil {
		t.Fatalf("re-storing a key must replace the existing item: %v", err)
	}
}

func TestStoreRejectsAMisSizedKey(t *testing.T) {
	if err := StoreEncryptionKey([]byte("short")); err == nil {
		t.Fatal("StoreEncryptionKey accepted a 5-byte key")
	}
}
