package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/puremetricsai/lumi/internal/macosnative"
	"github.com/puremetricsai/lumi/internal/seal"
	"github.com/puremetricsai/lumi/internal/store"
)

// The Keychain is the authority on whether encryption is on, and the file
// headers only report how far a conversion got.
//
// Reading intent off a header instead breaks the case that matters most. Turn
// encryption on before anything has been recorded and there is no lumi.db at
// all: a header check reads "plaintext", openStore creates a plaintext database,
// and the next keyed writer fails with "file is not a database" on a store the
// user believes has been encrypted from the first frame.
//
// So `encryptionEnabled` asks the Keychain, and it asks for attributes only, so
// refusing a command costs nobody a Keychain prompt. `store.FileIsEncrypted`
// exists to spot the disagreement — a key with a plaintext database, or the
// reverse — which is a conversion that died halfway and is `lumi doctor`'s to
// report and `lumi encrypt` to finish.

// keyring is the Keychain seam. It is a package var so tests can exercise every
// state — on, off, half-converted — without a real Keychain and without leaving
// an item behind on the developer's machine.
var keyring = keychain{
	has:    macosnative.HasEncryptionKey,
	load:   macosnative.EncryptionKey,
	store:  macosnative.StoreEncryptionKey,
	delete: macosnative.DeleteEncryptionKey,
}

type keychain struct {
	has    func() (bool, error)
	load   func() ([]byte, error)
	store  func([]byte) error
	delete func() error
}

// keys are the two derived keys a command needs to read anything.
//
// They come as a pair because a command that can open the database can almost
// always also be handed sealed media, and deriving them separately at each call
// site is how the two would eventually be derived from different masters.
type keys struct {
	database []byte
	media    seal.Key
}

// enabled reports whether these keys do anything, which is the same question as
// whether encryption is on.
func (k keys) enabled() bool { return k.media.Enabled() }

// keysFor resolves the keys to open one particular store.
//
// The Keychain holds one key per *user*, not per data directory — deliberately,
// so Storage settings' "Choose…" relocation cannot produce a store nothing can
// decrypt. The consequence is that a stored key says nothing about whether the
// database in front of this process is encrypted: a second `--data-dir` may be
// a plaintext store this user also has, and handing it a key opens it as
// "file is not a database".
//
// So the *file* decides, and the Keychain only supplies. A database that is
// encrypted needs the key; one that is plaintext is opened plaintext whatever
// the Keychain holds; and one that does not exist yet is created under the key
// if there is one, which is what makes encrypting a fresh install work.
func keysFor(databasePath string) (keys, error) {
	encrypted, err := store.FileIsEncrypted(databasePath)
	if err != nil {
		return keys{}, err
	}
	if !encrypted {
		if _, statErr := os.Stat(databasePath); statErr == nil {
			// A real, plaintext database. Not this user's encrypted store.
			return keys{}, nil
		}
	}
	k, err := resolveKeys()
	if err != nil {
		return keys{}, err
	}
	if encrypted && !k.enabled() {
		return keys{}, fmt.Errorf("the index at %s is encrypted and its key is not in this Mac's "+
			"Keychain, so it cannot be read", databasePath)
	}
	return k, nil
}

// storeIsProtected reports whether the database at this path is one the
// Keychain key opens — which is exactly the set of stores the content guard has
// something to protect.
//
// It never reads the key, only asks whether one exists, so refusing a command
// costs nobody a Keychain prompt.
func storeIsProtected(databasePath string) (bool, error) {
	encrypted, err := store.FileIsEncrypted(databasePath)
	if err != nil {
		return false, err
	}
	if encrypted {
		return true, nil
	}
	if _, err := os.Stat(databasePath); err == nil {
		// A real, plaintext database: already readable by anything, so a
		// refusal would protect nothing.
		return false, nil
	}
	// No database yet. If a key exists the next one created is encrypted, so
	// treat it as protected rather than printing from a store that is about to
	// become one.
	return keyring.has()
}

// resolveKeys reads the master key and derives from it, or returns the zero
// value when encryption is off.
//
// This is the one call that can prompt, so it belongs only on the paths that
// genuinely have to decrypt.
func resolveKeys() (keys, error) {
	master, err := keyring.load()
	if errors.Is(err, macosnative.ErrNoEncryptionKey) {
		return keys{}, nil
	}
	if err != nil {
		return keys{}, err
	}
	database, err := seal.DeriveDB(master)
	if err != nil {
		return keys{}, err
	}
	media, err := seal.DeriveMedia(master)
	if err != nil {
		return keys{}, err
	}
	return keys{database: database, media: media}, nil
}

// encryptionState is what `lumi encrypt status` reports, what `lumi doctor`
// checks, and what the app's toggle reads.
//
// "Is this store encrypted" is the *file's* answer, not the Keychain's. The
// Keychain holds one key per user, so a second `--data-dir` that this user keeps
// in plaintext is simply not encrypted — reporting it as a half-finished
// conversion because a key exists elsewhere was a diagnosis the evidence does
// not support, and it named a repair the user does not need.
//
// A store with no database yet is reported by the key, because that is the state
// a fresh install is in immediately after the toggle and the next database it
// creates really will be encrypted.
type encryptionState struct {
	// Enabled is whether this store is encrypted.
	Enabled bool `json:"enabled"`
	// DatabaseEncrypted is the file's own header. It differs from Enabled only
	// before anything has been recorded.
	DatabaseEncrypted bool `json:"database_encrypted"`
	// KeyPresent is whether a key exists at all. Enabled without it is the one
	// unrecoverable state.
	KeyPresent bool `json:"key_present"`
	// Database is the path all of this describes, because `--data-dir` means a
	// process can be looking at a store the user did not expect.
	Database string `json:"database"`
}

// Unrecoverable reports the one state nothing can fix: this store is encrypted
// and the key that opens it is gone.
func (s encryptionState) Unrecoverable() bool {
	return s.DatabaseEncrypted && !s.KeyPresent
}

func readEncryptionState(databasePath string) (encryptionState, error) {
	encrypted, err := store.FileIsEncrypted(databasePath)
	if err != nil {
		return encryptionState{}, err
	}
	present, err := keyring.has()
	if err != nil {
		return encryptionState{}, err
	}
	enabled, err := storeIsProtected(databasePath)
	if err != nil {
		return encryptionState{}, err
	}
	return encryptionState{
		Enabled:           enabled,
		DatabaseEncrypted: encrypted,
		KeyPresent:        present,
		Database:          databasePath,
	}, nil
}

// errEncryptedContent is what a content-emitting command refuses with.
//
// It names the app rather than another command, because there is no `lumi` on
// anyone's PATH: Lumi.app is the whole product and telling somebody to open a
// terminal is telling them to do something impossible.
var errEncryptedContent = errors.New(
	"this command prints captured screen text and transcripts, and Lumi's history is encrypted.\n" +
		"An AI assistant can still read it through Lumi's MCP server, which decrypts in memory and\n" +
		"never writes plaintext to a terminal. To read one event's screenshot or audio yourself, use\n" +
		"`lumi reveal <event-id>`. To turn encryption off, open Lumi → Settings → Storage")
