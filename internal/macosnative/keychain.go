//go:build darwin && arm64 && cgo

package macosnative

/*
#cgo CFLAGS: -fobjc-arc -Wno-deprecated-declarations
#cgo LDFLAGS: -framework CoreFoundation -framework Foundation -framework Security
#include <stdlib.h>
#include "keychain.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// Keychain access for Lumi's encryption key.
//
// This lives here for the same reason every other file in this package does: it
// is a framework call that needs cgo, and the rest of Lumi is written against a
// Go function rather than against Security.framework.
//
// It uses the **legacy file-based keychain**, not the data-protection one, and
// that is a measured choice rather than an oversight. The data-protection
// keychain would be better — it refuses other processes outright instead of
// prompting — but its access control is by application-identifier entitlement,
// which an ad-hoc-signed build does not carry: SecItemAdd returns
// errSecMissingEntitlement (-34018), so every development build would be unable
// to store a key at all. The legacy keychain takes a SecAccess ACL naming this
// binary, which is the per-binary control the feature actually wants, and it
// works ad-hoc. Its cost is that kSecAttrAccessible* is ignored there.
//
// Measured, with an ad-hoc signature: this binary stores and reads the key with
// no prompt; /usr/bin/security reading the *data* blocks on a user prompt.
// Reading only the *attributes* is ungated for anyone — which is exactly why
// HasEncryptionKey asks for attributes and never for data.
//
// Nothing in this file may write to stdout. `lumi mcp` calls EncryptionKey on
// startup and a single stray byte corrupts the JSON-RPC stream for the session.

// ErrNoEncryptionKey reports that no key is stored.
//
// It is a distinct error because "encryption is off" and "the Keychain could not
// be reached" are different answers, and collapsing them would let a transient
// failure read as a decision the user made.
var ErrNoEncryptionKey = errors.New("no Lumi encryption key is stored in the Keychain")

// EncryptionKeyLen is the length of the master key this package stores.
const EncryptionKeyLen = 32

// StoreEncryptionKey writes key to the Keychain, replacing any existing one.
//
// The item is ACL'd to this binary's code identity: another process asking for
// its data gets a user prompt rather than the bytes.
func StoreEncryptionKey(key []byte) error {
	if len(key) != EncryptionKeyLen {
		return fmt.Errorf("encryption key is %d bytes, want %d", len(key), EncryptionKeyLen)
	}
	status := C.LumiKeychainStore((*C.uint8_t)(unsafe.Pointer(&key[0])), C.size_t(len(key)))
	if status != 0 {
		return fmt.Errorf("store the encryption key in the Keychain: %s", keychainMessage(status))
	}
	return nil
}

// EncryptionKey reads the stored key, or reports ErrNoEncryptionKey.
//
// This is the call that can prompt, so it belongs on the paths that genuinely
// need to decrypt and nowhere else. Anything that only needs to know *whether*
// encryption is on must use HasEncryptionKey.
func EncryptionKey() ([]byte, error) {
	var buffer [EncryptionKeyLen]C.uint8_t
	var length C.size_t
	status := C.LumiKeychainLoad(&buffer[0], C.size_t(len(buffer)), &length)
	switch {
	case status == C.errSecItemNotFound:
		return nil, ErrNoEncryptionKey
	case status != 0:
		return nil, fmt.Errorf("read the encryption key from the Keychain: %s", keychainMessage(status))
	case int(length) != EncryptionKeyLen:
		return nil, fmt.Errorf("the stored encryption key is %d bytes, want %d", int(length), EncryptionKeyLen)
	}
	key := make([]byte, EncryptionKeyLen)
	for i := range key {
		key[i] = byte(buffer[i])
	}
	return key, nil
}

// HasEncryptionKey reports whether a key is stored, without reading it.
//
// It queries attributes only. That distinction is the whole reason this
// function exists rather than callers testing EncryptionKey for an error:
// asking for the data is gated by the ACL and prompts, so a `lumi search` that
// only needs to know it should refuse would otherwise pop a Keychain dialog on
// its way to printing an error.
func HasEncryptionKey() (bool, error) {
	status := C.LumiKeychainHas()
	switch {
	case status == 0:
		return true, nil
	case status == C.errSecItemNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("look for the encryption key in the Keychain: %s", keychainMessage(status))
	}
}

// DeleteEncryptionKey removes the stored key. Removing one that is not there is
// success: `lumi encrypt off` must be re-runnable after a crash.
func DeleteEncryptionKey() error {
	status := C.LumiKeychainDelete()
	if status != 0 && status != C.errSecItemNotFound {
		return fmt.Errorf("remove the encryption key from the Keychain: %s", keychainMessage(status))
	}
	return nil
}

func keychainMessage(status C.int32_t) string {
	message := C.LumiKeychainMessage(status)
	if message == nil {
		return fmt.Sprintf("OSStatus %d", int(status))
	}
	defer C.free(unsafe.Pointer(message))
	return fmt.Sprintf("%s (OSStatus %d)", C.GoString(message), int(status))
}
