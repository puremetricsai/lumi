// Package seal encrypts captured media files in place.
//
// It holds one format and the six operations Lumi performs on it, and nothing
// else: no database, no filesystem layout, no knowledge of what a screenshot or
// a WAV is. That is deliberate — like internal/wav and internal/transcript it is
// pure Go with no build tag, so the rules it enforces are exercisable anywhere.
//
// The format is a magic header, a nonce, and AES-256-GCM:
//
//	"LUMIENC1" | 12-byte nonce | ciphertext+tag
//
// The header is the load-bearing part. Every reader detects for itself whether a
// file is sealed, which means a directory half-way through conversion is a
// *correct* state rather than a broken one: `lumi encrypt` can be killed and
// re-run, it skips what it already did, and nothing has to write down where it
// got to. There is no journal because the files are the journal.
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Magic prefixes every sealed file.
const Magic = "LUMIENC1"

// KeyLen is the length of a derived key. It is AES-256's.
const KeyLen = 32

// ScratchSuffix names the temporary file a seal writes before renaming it over
// its target.
//
// It is exported because two other packages have to recognise it: a crash
// mid-seal leaves one beside the media, where internal/compress's reconcile
// walk would read it as an orphaned encode and `lumi encrypt`'s resume would try
// to convert it. Naming it here is what stops either of them guessing.
const ScratchSuffix = ".lumi-sealing"

// ErrNotSealed reports that a file does not carry the magic header.
var ErrNotSealed = errors.New("file is not sealed")

// Key is the media key. The zero value — nil — is a working pass-through, so
// every caller is written once and behaves correctly whether or not encryption
// is on. That is the whole reason this is a type rather than a []byte parameter.
type Key []byte

// Enabled reports whether this key does anything. Callers use it where the
// encrypted and plaintext paths genuinely differ — internal/compress encodes
// through a temporary file only when there is something to seal — rather than
// paying for a copy that would be a no-op.
func (k Key) Enabled() bool { return len(k) > 0 }

func (k Key) aead() (cipher.AEAD, error) {
	if len(k) != KeyLen {
		return nil, fmt.Errorf("media key is %d bytes, want %d", len(k), KeyLen)
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, fmt.Errorf("media cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

// DeriveDB and DeriveMedia split one stored master key into the two the product
// uses.
//
// One secret is stored and two are used, so a weakness or a format change in
// either half cannot reach the other. HKDF is four lines and removes the
// question of whether reusing one key across two constructions is safe.
func DeriveDB(master []byte) ([]byte, error) {
	return derive(master, "lumi-db-v1")
}

func DeriveMedia(master []byte) (Key, error) {
	key, err := derive(master, "lumi-media-v1")
	return Key(key), err
}

func derive(master []byte, info string) ([]byte, error) {
	if len(master) == 0 {
		return nil, nil
	}
	key, err := hkdf.Key(sha256.New, master, nil, info, KeyLen)
	if err != nil {
		return nil, fmt.Errorf("derive %s key: %w", info, err)
	}
	return key, nil
}

// IsSealed reports whether the file at path carries the magic header.
//
// A file too short to hold one is not sealed — that is a truncated or empty
// capture, and treating it as sealed would send it to a decrypt that fails
// instead of to a reader that can say what is actually wrong.
func IsSealed(path string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	header := make([]byte, len(Magic))
	if _, err := io.ReadFull(file, header); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return false, nil
		}
		return false, err
	}
	return string(header) == Magic, nil
}

// ReadFile returns a media file's plaintext, unsealing it if it is sealed.
//
// It reads a plaintext file whether or not the key is set, because during a
// conversion — and for the seconds between a capture being written and being
// sealed — both kinds are on disk at once, and a reader that insisted on one
// would fail on exactly the files it is most important not to lose.
func (k Key) ReadFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !hasMagic(raw) {
		return raw, nil
	}
	if !k.Enabled() {
		return nil, fmt.Errorf("%s is sealed and no key is available", path)
	}
	return k.open(raw, path)
}

func (k Key) open(raw []byte, path string) ([]byte, error) {
	aead, err := k.aead()
	if err != nil {
		return nil, err
	}
	body := raw[len(Magic):]
	if len(body) < aead.NonceSize() {
		return nil, fmt.Errorf("%s is sealed but too short to hold a nonce", path)
	}
	plain, err := aead.Open(nil, body[:aead.NonceSize()], body[aead.NonceSize():], nil)
	if err != nil {
		// Never name the cipher error: on a wrong key it is indistinguishable
		// from a tampered file, and claiming corruption when the key is simply
		// the wrong one sends the user to recover data that is intact.
		return nil, fmt.Errorf("could not decrypt %s; the key may be wrong or the file damaged", path)
	}
	return plain, nil
}

func hasMagic(raw []byte) bool {
	return len(raw) >= len(Magic) && string(raw[:len(Magic)]) == Magic
}

// SealFile encrypts the file at path in place, leaving its name unchanged.
//
// The name is deliberate: internal/compress classifies work by extension,
// internal/capture pairs a file with an event by swapping one, and
// ReadAudioEnvelope dispatches on `.wav`. A sealed screenshot is still `.jpg`.
//
// It is a no-op when the key is empty or the file is already sealed, which is
// what makes `lumi encrypt on` resumable and re-runnable.
func (k Key) SealFile(path string) error {
	if !k.Enabled() {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if hasMagic(raw) {
		return nil
	}
	sealed, err := k.seal(raw)
	if err != nil {
		return err
	}
	return replaceDurably(path, sealed)
}

// UnsealFile is SealFile's inverse and is a no-op on a file that is not sealed.
func (k Key) UnsealFile(path string) error {
	if !k.Enabled() {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !hasMagic(raw) {
		return nil
	}
	plain, err := k.open(raw, path)
	if err != nil {
		return err
	}
	return replaceDurably(path, plain)
}

// SealInto writes a sealed copy of source at destination, which must not exist.
//
// This is internal/compress's write side: it encodes into a plaintext temporary
// file and then needs the result to land, sealed, at the path an event row will
// be repointed at. Sealing in place would mean the destination existed
// unsealed first, which is the one moment compress cannot have.
func (k Key) SealInto(source, destination string) error {
	raw, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if !k.Enabled() {
		return writeNew(destination, raw)
	}
	sealed, err := k.seal(raw)
	if err != nil {
		return err
	}
	return writeNew(destination, sealed)
}

func (k Key) seal(plain []byte) ([]byte, error) {
	aead, err := k.aead()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	out := make([]byte, 0, len(Magic)+len(nonce)+len(plain)+aead.Overhead())
	out = append(out, Magic...)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plain, nil), nil
}

// TempCopy hands back a plaintext path for an API that takes paths rather than
// bytes — Vision OCR, SpeechAnalyzer, and the HEIC and FLAC encoders all do.
//
// With no key, or on a file that is not sealed, it returns the original path
// and a cleanup that does nothing, so no caller needs a branch.
//
// The copy goes to the system temporary directory, never beside the media.
// internal/compress reads an unrecognised sibling as an orphaned encode and
// internal/retention's --all sweep deletes any unreferenced file it finds, so a
// plaintext temporary parked in the media directory is data loss waiting for the
// next prune.
//
// ponytail: the plaintext copy lands on the boot volume even when the data
// directory is on an external one. Route it through the data directory's own
// volume if that ever matters; it costs a second temp-directory policy.
func (k Key) TempCopy(path string) (string, func(), error) {
	noop := func() {}
	if !k.Enabled() {
		return path, noop, nil
	}
	sealed, err := IsSealed(path)
	if err != nil {
		return "", noop, err
	}
	if !sealed {
		return path, noop, nil
	}
	plain, err := k.ReadFile(path)
	if err != nil {
		return "", noop, err
	}
	dir, err := os.MkdirTemp("", "lumi-")
	if err != nil {
		return "", noop, fmt.Errorf("temporary directory: %w", err)
	}
	cleanup := func() { os.RemoveAll(dir) }
	// The base name is preserved because the callers that need this are the
	// ones that dispatch on extension.
	temp := filepath.Join(dir, filepath.Base(path))
	if err := os.WriteFile(temp, plain, 0o600); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("temporary copy: %w", err)
	}
	return temp, cleanup, nil
}

// replaceDurably writes content over path without ever leaving it partial.
//
// The scratch file is a sibling so the rename is atomic within one filesystem,
// and both it and the directory are flushed before the rename, because the
// caller is overwriting the only copy of a captured file.
func replaceDurably(path string, content []byte) error {
	scratch := path + ScratchSuffix
	if err := writeNew(scratch, content); err != nil {
		return err
	}
	if err := os.Rename(scratch, path); err != nil {
		os.Remove(scratch)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return SyncDir(filepath.Dir(path))
}

func writeNew(path string, content []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		os.Remove(path)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(path)
		return fmt.Errorf("flush %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

// SyncDir flushes a directory entry.
//
// Exported because renaming a file into place is a two-part commit — the bytes
// and the *name* — and the second half is easy to leave out. `lumi encrypt`
// does the same rename over the database that this package does over a media
// file, so it reads the primitive from here rather than keeping a second copy.
func SyncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
