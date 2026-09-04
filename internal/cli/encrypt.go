package cli

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/puremetricsai/lumi/internal/config"
	"github.com/puremetricsai/lumi/internal/macosnative"
	"github.com/puremetricsai/lumi/internal/seal"
	"github.com/puremetricsai/lumi/internal/store"
)

// `lumi encrypt` converts a data directory between plaintext and encrypted.
//
// The ordering is the whole design, and it is asymmetric on purpose.
//
// Turning encryption **on**: store the key first, then seal the media, then
// convert the database. The key goes first because sealing a file against a key
// that was never persisted destroys it. The database goes last because
// `encrypt status` reads the database header, so converting it first would
// report a finished conversion while months of media were still plaintext.
//
// Turning it **off**: convert the database, then unseal the media, then delete
// the key — last, so it outlives every file that still needs it.
//
// Neither direction writes a journal or a progress file. The magic header on a
// media file and the SQLite header on the database *are* the record of what has
// been done, so a run that is killed halfway leaves a directory that every
// reader handles correctly and that a re-run finishes. That is the entire payoff
// of putting a header on the format.
func (a *app) encryptCommand() *cobra.Command {
	cmd := emitsNoContent(&cobra.Command{
		Use:   "encrypt",
		Short: "Encrypt Lumi's captured history, or turn encryption off",
		Long: "Encrypt the screenshots, audio, and search index in the data directory.\n\n" +
			"The key is generated on this Mac and stored in the login Keychain, locked to Lumi's\n" +
			"own code identity: another program asking for it gets a system prompt. Nothing else\n" +
			"can read the captured history from disk, and Lumi's own `search` and `transcript`\n" +
			"commands stop printing it — an AI assistant reaches it through the MCP server, which\n" +
			"decrypts in memory.\n\n" +
			"If the Keychain item is lost, the captured history is unrecoverable. There is no\n" +
			"password, no recovery code, and no second copy.",
	})
	cmd.AddCommand(
		a.encryptDirectionCommand("on", "Encrypt the data directory", true),
		a.encryptDirectionCommand("off", "Decrypt the data directory and forget the key", false),
		a.encryptStatusCommand())
	return cmd
}

// encryptDirectionCommand builds `on` and `off`, which differ only in which way
// they convert.
func (a *app) encryptDirectionCommand(use, short string, on bool) *cobra.Command {
	var asJSON bool
	cmd := emitsNoContent(&cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runEncrypt(cmd.Context(), cmd, asJSON, on)
		},
	})
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the result as JSON")
	return cmd
}

func (a *app) encryptStatusCommand() *cobra.Command {
	var asJSON bool
	cmd := emitsNoContent(&cobra.Command{
		Use:   "status",
		Short: "Report whether the data directory is encrypted",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := a.paths()
			if err != nil {
				return err
			}
			state, err := readEncryptionState(paths.Database)
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(state)
			}
			return renderEncryptionState(cmd, state)
		},
	})
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the status as JSON")
	return cmd
}

// EncryptResult is what a conversion reports.
type EncryptResult struct {
	// Enabled is the state after the run.
	Enabled bool `json:"enabled"`
	// MediaConverted counts files this run sealed or unsealed; MediaSkipped
	// counts those already in the target state, which is what a resumed run
	// finds. They are separate because "nothing to do" and "did nothing" are
	// different answers.
	MediaConverted int `json:"media_converted"`
	MediaSkipped   int `json:"media_skipped"`
	// MediaFailed counts files that could not be converted. They keep their
	// current form and a later run retries them, because the header on each file
	// is the record of what is left.
	MediaFailed  int    `json:"media_failed"`
	DatabasePath string `json:"database_path"`
}

func (a *app) runEncrypt(ctx context.Context, cmd *cobra.Command, asJSON, on bool) error {
	paths, err := a.paths()
	if err != nil {
		return err
	}
	if err := paths.Ensure(); err != nil {
		return err
	}
	// Two locks, because two different things must be excluded.
	//
	// The capture lock is exclusive here and shared by every recorder, so a
	// recorder cannot start underneath a conversion and a conversion cannot
	// start underneath a recorder. Reading `record.json` is not enough on its
	// own — a recorder that starts after the check is invisible to it, and would
	// then write plaintext behind a walk that has already passed it and hold a
	// handle on a database about to be renamed away. The state file is still
	// read first, because "a recording is in progress" is a better message than
	// "the lock is held".
	if err := refuseEncryptWhileRecording(paths); err != nil {
		return err
	}
	releaseCapture, err := lockCapture(paths)
	if err != nil {
		return err
	}
	defer releaseCapture()
	sweepAbandonedPlaintext(cmd.ErrOrStderr())
	// The compress lock, for the same reason `lumi compress` takes it: both
	// rewrite media in place, and two of them on one file is the state neither
	// ordering survives.
	release, err := lockCompress(paths)
	if err != nil {
		return err
	}
	defer release()

	state, err := readEncryptionState(paths.Database)
	if err != nil {
		return err
	}
	// `on` never refuses for being already on. The headers are this command's
	// only record of what is done, and they are per file — so a store whose key
	// and database agree can still hold plaintext media: a seal that failed at
	// capture time is logged and left readable on purpose (the never-lose-media
	// rule), on the promise that the next run picks it up. Refusing here made
	// that promise false, and the file stayed readable forever with nothing
	// saying so. Re-running is idempotent by construction; let it run and report
	// what it found.
	if !on && !state.Enabled && !state.KeyPresent {
		return errors.New("Lumi's history is not encrypted")
	}
	if !on && state.Unrecoverable() {
		return errors.New("the database is encrypted and its key is not in this Mac's Keychain, " +
			"so it cannot be decrypted; there is no way to recover it")
	}

	result := EncryptResult{Enabled: on, DatabasePath: paths.Database}
	if on {
		err = a.encryptOn(ctx, cmd, paths, state, &result)
	} else {
		err = a.encryptOff(ctx, cmd, paths, &result)
	}
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}
	return renderEncryptResult(cmd, result, on)
}

func (a *app) encryptOn(ctx context.Context, cmd *cobra.Command, paths config.Paths,
	state encryptionState, result *EncryptResult) error {
	// The key is persisted before a single byte is sealed. A file encrypted
	// under a key that was never written down is gone, and no ordering after
	// that point can get it back.
	k, err := ensureKey()
	if err != nil {
		return err
	}
	if err := convertMedia(ctx, cmd, paths, k.media, true, result); err != nil {
		return err
	}
	// The database last: `encrypt status` reads its header, so converting it
	// first would report a finished job while the media was still plaintext.
	if !state.DatabaseEncrypted {
		if err := convertDatabase(ctx, paths.Database, nil, k.database); err != nil {
			return err
		}
	}
	// Converting the database is what makes the store read as encrypted, so a
	// run that left plaintext media behind must not exit reporting success —
	// every status surface would then say "on" over files anyone can read.
	// Re-running repairs it; sealed files are skipped by their header.
	if result.MediaFailed > 0 {
		return fmt.Errorf("%d file(s) could not be encrypted and are still readable on disk; "+
			"run `lumi encrypt on` again to retry them", result.MediaFailed)
	}
	return nil
}

func (a *app) encryptOff(ctx context.Context, cmd *cobra.Command, paths config.Paths,
	result *EncryptResult) error {
	k, err := resolveKeys()
	if err != nil {
		return err
	}
	if !k.enabled() {
		return errors.New("Lumi's encryption key is not in this Mac's Keychain")
	}
	encrypted, err := store.FileIsEncrypted(paths.Database)
	if err != nil {
		return err
	}
	if encrypted {
		if err := convertDatabase(ctx, paths.Database, k.database, nil); err != nil {
			return err
		}
	}
	if err := convertMedia(ctx, cmd, paths, k.media, false, result); err != nil {
		return err
	}
	// The key goes last, and only if every file made it back. Deleting it while
	// anything is still sealed destroys that file permanently — this is the one
	// irreversible step in the command, so a partial success may not reach it.
	// The run is re-runnable: what is already unsealed is skipped by its header.
	if result.MediaFailed > 0 {
		return fmt.Errorf("%d file(s) could not be decrypted, so Lumi's encryption key has been "+
			"kept — deleting it would destroy them. Fix the cause and run `lumi encrypt off` again",
			result.MediaFailed)
	}
	if err := keyring.delete(); err != nil {
		// Everything is already decrypted at this point, so a key that will not
		// delete is orphaned rather than dangerous — and it happens for a
		// mundane reason: the item's ACL names the binary that created it, and a
		// rebuild or a rotated signing certificate makes this a different one.
		// Failing the command here would report a decryption that succeeded as
		// an error, and send the user looking for data loss that did not happen.
		fmt.Fprintf(cmd.ErrOrStderr(),
			"Lumi's history is decrypted, but the old key could not be removed from the Keychain: %v\n"+
				"It no longer opens anything. Delete the \"Lumi captured history\" item in Keychain "+
				"Access if you want it gone.\n", err)
	}
	return nil
}

// ensureKey returns the stored key, generating and storing one if there is none.
//
// Reusing an existing key matters: a conversion killed halfway leaves sealed
// files behind, and minting a fresh key on the retry would make them
// permanently unreadable while the run reported success.
func ensureKey() (keys, error) {
	k, err := resolveKeys()
	if err != nil {
		return keys{}, err
	}
	if k.enabled() {
		return k, nil
	}
	master := make([]byte, macosnative.EncryptionKeyLen)
	if _, err := rand.Read(master); err != nil {
		return keys{}, fmt.Errorf("generate an encryption key: %w", err)
	}
	if err := keyring.store(master); err != nil {
		return keys{}, err
	}
	return resolveKeys()
}

// convertDatabase writes a converted copy beside the database and renames it
// into place.
//
// The scratch name is fixed rather than random, and it is removed first, so a
// run killed between writing and renaming leaves nothing a later run has to
// reason about — it simply overwrites it.
func convertDatabase(ctx context.Context, path string, from, to []byte) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		// Nothing has been recorded yet. The next open creates the database
		// under whichever key is now in the Keychain, so there is nothing to
		// convert and reporting an error here would refuse to encrypt a fresh
		// install — the case where it is easiest to say yes.
		return nil
	} else if err != nil {
		return err
	}

	scratch := path + ".converting"
	if err := os.Remove(scratch); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear a previous conversion: %w", err)
	}
	if err := store.ConvertTo(ctx, path, from, scratch, to); err != nil {
		return err
	}
	// The write-ahead log holds pages of the *old* database in the old form.
	// Renaming over lumi.db without clearing it leaves a plaintext -wal beside
	// an encrypted database — which is both a leak and a corrupt pair, and
	// neither is visible from the file that was replaced.
	if err := clearWAL(path); err != nil {
		os.Remove(scratch)
		return err
	}
	if err := os.Rename(scratch, path); err != nil {
		os.Remove(scratch)
		return fmt.Errorf("replace the database with its conversion: %w", err)
	}
	return seal.SyncDir(filepath.Dir(path))
}

// clearWAL removes the sidecars belonging to the database being replaced.
//
// store.ConvertTo has already opened and closed the source, which checkpoints
// and unlinks them in the ordinary case; this covers the case where a previous
// process died holding them.
func clearWAL(path string) error {
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("clear %s%s: %w", filepath.Base(path), suffix, err)
		}
	}
	return nil
}

// convertMedia seals or unseals every captured file.
//
// A file already in the target state is skipped by reading its header, which is
// what makes this resumable. A file that fails is counted and left alone: it
// keeps whichever form it has, every reader still handles it, and the next run
// tries again. Failing the whole conversion over one unreadable file would leave
// the directory in exactly the mixed state it is trying to leave, minus the
// chance to finish the rest.
func convertMedia(ctx context.Context, cmd *cobra.Command, paths config.Paths,
	key seal.Key, sealing bool, result *EncryptResult) error {
	for _, dir := range []string{paths.Screenshots, paths.Audio} {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("scan %s: %w", dir, err)
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.IsDir() {
				continue
			}
			// A scratch file from a seal that was interrupted is not captured
			// media. Converting one would produce a sealed fragment that looks
			// like a real capture to the next run.
			if filepath.Ext(entry.Name()) == seal.ScratchSuffix {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			done, err := seal.IsSealed(path)
			if err != nil {
				result.MediaFailed++
				continue
			}
			if done == sealing {
				result.MediaSkipped++
				continue
			}
			if sealing {
				err = key.SealFile(path)
			} else {
				err = key.UnsealFile(path)
			}
			if err != nil {
				result.MediaFailed++
				fmt.Fprintf(cmd.ErrOrStderr(), "could not convert %s: %v\n", path, err)
				continue
			}
			result.MediaConverted++
		}
	}
	return nil
}

// refuseEncryptWhileRecording stops a conversion racing the recorder.
//
// The recorder writes media and rows continuously, so converting underneath it
// means files appearing in the old form behind a walk that has already passed
// them — and a database renamed out from under a live handle.
func refuseEncryptWhileRecording(paths config.Paths) error {
	state, ok, err := readRecordState(paths)
	if err != nil {
		return err
	}
	if ok && processAlive(state.PID) {
		return errors.New("a recording is in progress; stop it before changing encryption " +
			"(Lumi's menu bar, or `lumi record stop`)")
	}
	return nil
}

func renderEncryptResult(cmd *cobra.Command, result EncryptResult, on bool) error {
	out := cmd.OutOrStdout()
	verb := "Encrypted"
	if !on {
		verb = "Decrypted"
	}
	fmt.Fprintf(out, "%s Lumi's history: %d files converted", verb, result.MediaConverted)
	if result.MediaSkipped > 0 {
		fmt.Fprintf(out, ", %d already done", result.MediaSkipped)
	}
	if result.MediaFailed > 0 {
		fmt.Fprintf(out, ", %d could not be converted (run this again to retry them)", result.MediaFailed)
	}
	fmt.Fprintln(out, ".")
	if on {
		fmt.Fprintln(out, "The key is in this Mac's login Keychain. If it is lost, nothing can recover "+
			"the captured history.")
	}
	return nil
}

func renderEncryptionState(cmd *cobra.Command, state encryptionState) error {
	out := cmd.OutOrStdout()
	switch {
	case state.Unrecoverable():
		fmt.Fprintln(out, "Encryption: BROKEN — the database is encrypted and its key is not in this "+
			"Mac's Keychain. The captured history cannot be read.")
	case state.Enabled:
		fmt.Fprintln(out, "Encryption: on")
	default:
		fmt.Fprintln(out, "Encryption: off")
	}
	fmt.Fprintf(out, "Database: %s\n", state.Database)
	return nil
}
