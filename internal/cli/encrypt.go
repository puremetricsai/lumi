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
// On: store the key, seal the media, then convert the database. Off: convert
// the database, unseal the media, then delete the key. The key is written before
// the first file needs it and deleted after the last file stops needing it, and
// the database is converted last on the way in, so a run that could not seal
// everything leaves a plaintext database that `encrypt status` reports as
// incomplete rather than a finished one.
//
// There is no journal: the header on each file is the record of what is done,
// so a run killed halfway is finished by running either direction again.
func (a *app) encryptCommand() *cobra.Command {
	cmd := &cobra.Command{
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
	}
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
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runEncrypt(cmd.Context(), cmd, asJSON, on)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the result as JSON")
	return cmd
}

func (a *app) encryptStatusCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report whether the data directory is encrypted",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := a.paths()
			if err != nil {
				return err
			}
			state, err := readEncryptionState(paths)
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(state)
			}
			return renderEncryptionState(cmd, state)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the status as JSON")
	return cmd
}

// EncryptResult is what a successful conversion reports. A conversion that
// leaves anything unconverted fails instead, so there are no counts to read.
type EncryptResult struct {
	Enabled bool `json:"enabled"`
}

func (a *app) runEncrypt(ctx context.Context, cmd *cobra.Command, asJSON, on bool) error {
	paths, err := a.paths()
	if err != nil {
		return err
	}
	if err := paths.Ensure(); err != nil {
		return err
	}
	// The capture lock is exclusive here and shared by every recorder, so neither
	// can start underneath the other — including a recorder the app starts after
	// it quit mid-conversion and was reopened. `record.json` is read first only
	// for the better message.
	if err := refuseEncryptWhileRecording(paths); err != nil {
		return err
	}
	releaseCapture, err := lockCapture(paths)
	if err != nil {
		return err
	}
	defer releaseCapture()
	// Both this and `lumi compress` rewrite media in place.
	release, err := lockCompress(paths)
	if err != nil {
		return err
	}
	defer release()

	if on {
		err = encryptOn(ctx, paths)
	} else {
		err = encryptOff(ctx, cmd, paths)
	}
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(EncryptResult{Enabled: on})
	}
	if on {
		fmt.Fprintln(cmd.OutOrStdout(), "Encrypted Lumi's history. The key is in this Mac's login Keychain; "+
			"if it is lost, nothing can recover the captured history.")
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "Decrypted Lumi's history.")
	}
	return nil
}

func encryptOn(ctx context.Context, paths config.Paths) error {
	k, err := ensureKey()
	if err != nil {
		return err
	}
	// `on` never refuses for being already on: a seal that failed at capture
	// time leaves a readable file in an encrypted store, and re-running is what
	// picks it up. Sealed files are skipped by their header.
	if err := convertMedia(ctx, paths, k.media, true); err != nil {
		return err
	}
	encrypted, err := store.FileIsEncrypted(paths.Database)
	if err != nil || encrypted {
		return err
	}
	return convertDatabase(ctx, paths.Database, nil, k.database)
}

func encryptOff(ctx context.Context, cmd *cobra.Command, paths config.Paths) error {
	encrypted, err := store.FileIsEncrypted(paths.Database)
	if err != nil {
		return err
	}
	k, err := resolveKeys()
	if err != nil {
		return err
	}
	if !k.enabled() {
		if encrypted {
			return errors.New("the database is encrypted and its key is not in this Mac's Keychain, " +
				"so it cannot be decrypted; there is no way to recover it")
		}
		return errors.New("Lumi's history is not encrypted")
	}
	if encrypted {
		if err := convertDatabase(ctx, paths.Database, k.database, nil); err != nil {
			return err
		}
	}
	// The key goes last, and only if every file made it back: deleting it while
	// anything is still sealed destroys that file.
	if err := convertMedia(ctx, paths, k.media, false); err != nil {
		return fmt.Errorf("%w; Lumi kept its encryption key, because deleting it would destroy them", err)
	}
	if err := keyring.delete(); err != nil {
		// Everything is already decrypted, so the key is orphaned rather than
		// dangerous. The usual cause is an ACL naming a binary a rebuild replaced.
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

// convertMedia seals or unseals every captured file, skipping those already in
// the target state. A file that fails keeps its current form, the rest are still
// converted, and the run then fails naming the first failure — so the caller
// never takes the next step over files left behind.
//
// Each directory is flushed once at the end rather than after every file. A
// rename lost to a crash leaves the original beside a complete scratch file,
// which the next attempt removes, so only the caller's next step — converting
// the database, or deleting the key — needs the renames to have landed.
func convertMedia(ctx context.Context, paths config.Paths, key seal.Key, sealing bool) error {
	failed := 0
	var first error
	err := eachMedia(paths, func(path string) (bool, error) {
		if err := ctx.Err(); err != nil {
			return true, err
		}
		var err error
		if sealing {
			err = key.SealFile(path)
		} else {
			err = key.UnsealFile(path)
		}
		if err != nil {
			if failed == 0 {
				first = err
			}
			failed++
		}
		return false, nil
	})
	if err != nil {
		return err
	}
	for _, dir := range []string{paths.Screenshots, paths.Audio} {
		if err := seal.SyncDir(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d file(s) could not be converted (first: %v)", failed, first)
	}
	return nil
}

// anyMediaSealed reports whether any captured file carries the seal header.
func anyMediaSealed(paths config.Paths) (bool, error) {
	found := false
	err := eachMedia(paths, func(path string) (bool, error) {
		sealed, err := seal.IsSealed(path)
		found = err == nil && sealed
		return found, nil
	})
	return found, err
}

// eachMedia calls visit for every captured media file until it returns true.
// A scratch file from an interrupted seal is not captured media and is skipped.
func eachMedia(paths config.Paths, visit func(path string) (stop bool, err error)) error {
	for _, dir := range []string{paths.Screenshots, paths.Audio} {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("scan %s: %w", dir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) == seal.ScratchSuffix {
				continue
			}
			stop, err := visit(filepath.Join(dir, entry.Name()))
			if stop || err != nil {
				return err
			}
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
		return errors.New("a recording is in progress; stop recording before changing encryption")
	}
	return nil
}

func renderEncryptionState(cmd *cobra.Command, state encryptionState) error {
	out := cmd.OutOrStdout()
	switch {
	case state.Unrecoverable:
		fmt.Fprintln(out, "Encryption: BROKEN — the database is encrypted and its key is not in this "+
			"Mac's Keychain. The captured history cannot be read.")
	case state.Incomplete:
		fmt.Fprintln(out, "Encryption: incomplete — a conversion stopped partway; run either direction to finish it")
	case state.Enabled:
		fmt.Fprintln(out, "Encryption: on")
	default:
		fmt.Fprintln(out, "Encryption: off")
	}
	fmt.Fprintf(out, "Database: %s\n", state.Database)
	return nil
}
