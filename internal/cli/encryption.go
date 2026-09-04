package cli

import (
	"errors"

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

// encryptionState is what `lumi encrypt status` reports and what `lumi doctor`
// checks. The two halves are separate fields because a conversion killed halfway
// leaves them disagreeing, and one merged boolean could only report that as a
// guess.
type encryptionState struct {
	// Enabled is the Keychain's answer: a key exists, so encryption is on.
	Enabled bool `json:"enabled"`
	// DatabaseEncrypted is the file's answer.
	DatabaseEncrypted bool `json:"database_encrypted"`
	// Database is the path the two describe, because `--data-dir` means a
	// process can be looking at a store the user did not expect.
	Database string `json:"database"`
}

// Incomplete reports a conversion that did not finish. It is never normal, and
// the fix is always to re-run `lumi encrypt`, which resumes.
func (s encryptionState) Incomplete() bool {
	return s.Enabled != s.DatabaseEncrypted
}

// Unrecoverable reports the one state nothing can fix: the database is
// encrypted and the key that opens it is gone.
func (s encryptionState) Unrecoverable() bool {
	return s.DatabaseEncrypted && !s.Enabled
}

func readEncryptionState(databasePath string) (encryptionState, error) {
	enabled, err := keyring.has()
	if err != nil {
		return encryptionState{}, err
	}
	encrypted, err := store.FileIsEncrypted(databasePath)
	if err != nil {
		return encryptionState{}, err
	}
	return encryptionState{Enabled: enabled, DatabaseEncrypted: encrypted, Database: databasePath}, nil
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
