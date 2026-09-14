package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/puremetricsai/lumi/internal/config"
	"github.com/puremetricsai/lumi/internal/macosnative"
	"github.com/puremetricsai/lumi/internal/seal"
	"github.com/puremetricsai/lumi/internal/store"
)

// Whether a store is encrypted is the database file's answer; the Keychain only
// supplies the key. The Keychain holds one key per user, not per data
// directory, so a stored key says nothing about the store in front of this
// process. The one case the file cannot answer is a store with no database yet,
// and there the key decides: the next database created is encrypted under it.

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
// checks, and what the app's toggle reads. Every judgement is made here, so the
// app displays these fields and derives nothing from them.
type encryptionState struct {
	// Enabled is whether this store is encrypted, or will be created encrypted.
	Enabled bool `json:"enabled"`
	// Incomplete is a conversion that stopped partway: a key and a plaintext
	// database, with media still sealed. Either direction finishes it.
	Incomplete bool `json:"incomplete"`
	// Unrecoverable is an encrypted database whose key is gone.
	Unrecoverable bool `json:"unrecoverable"`
	// Database is the path all of this describes, because `--data-dir` means a
	// process can be looking at a store the user did not expect.
	Database string `json:"database"`
}

// readEncryptionState never reads the key, only asks whether one exists, so
// checking costs nobody a Keychain prompt. It only walks the media for the
// rare key-with-plaintext-database case, which is how an interrupted run and a
// second plaintext `--data-dir` are told apart.
func readEncryptionState(paths config.Paths) (encryptionState, error) {
	state := encryptionState{Database: paths.Database}
	encrypted, err := store.FileIsEncrypted(paths.Database)
	if err != nil {
		return state, err
	}
	_, statErr := os.Stat(paths.Database)
	exists := statErr == nil
	present, err := keyring.has()
	if err != nil {
		return state, err
	}
	state.Enabled = encrypted || (!exists && present)
	state.Unrecoverable = encrypted && !present
	if present && exists && !encrypted {
		state.Incomplete, err = anyMediaSealed(paths)
	}
	return state, err
}

// errEncryptedContent is what a content-emitting command refuses with.
//
// It names the app rather than another command, because there is no `lumi` on
// anyone's PATH: Lumi.app is the whole product and telling somebody to open a
// terminal is telling them to do something impossible.
var errEncryptedContent = errors.New(
	"this command prints captured screen text and transcripts, and Lumi's history is encrypted.\n" +
		"An AI assistant can still read it through Lumi's MCP server, which decrypts in memory and\n" +
		"never writes plaintext to a terminal. To turn encryption off, open Lumi → Settings → Storage")
