//go:build !darwin || !arm64 || !cgo

package macosnative

import "errors"

// ErrNoEncryptionKey mirrors the darwin build's, so callers can compare against
// it without a build tag of their own.
var ErrNoEncryptionKey = errors.New("no Lumi encryption key is stored in the Keychain")

// EncryptionKeyLen mirrors the darwin build's.
const EncryptionKeyLen = 32

func StoreEncryptionKey([]byte) error { return errUnsupported }
func EncryptionKey() ([]byte, error)  { return nil, errUnsupported }
func HasEncryptionKey() (bool, error) { return false, errUnsupported }
func DeleteEncryptionKey() error      { return errUnsupported }
