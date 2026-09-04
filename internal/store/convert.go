package store

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/ncruces/go-sqlite3"
)

// ConvertTo writes the database at source out to destination, changing whether
// it is encrypted.
//
// Either key may be empty, so this covers both directions: plaintext to
// encrypted when destinationKey is set, encrypted to plaintext when only
// sourceKey is. It never touches source, and it verifies destination before
// returning — `lumi encrypt` renames the result over a database it is about to
// delete, so a conversion that silently produced a short file would take the
// index with it.
//
// destination must not exist. VACUUM INTO refuses to overwrite, and rather than
// work around that, the caller unlinks its scratch file first — which is what
// makes a conversion killed halfway simply re-runnable.
func ConvertTo(ctx context.Context, source string, sourceKey []byte, destination string, destinationKey []byte) error {
	if _, err := os.Stat(destination); err == nil {
		return fmt.Errorf("convert: %s already exists", destination)
	}

	src, err := Open(ctx, source, sourceKey)
	if err != nil {
		return fmt.Errorf("convert: open source: %w", err)
	}
	defer src.Close()

	target, err := vacuumTarget(destination, destinationKey)
	if err != nil {
		return err
	}
	// VACUUM INTO is ATTACH underneath, and ATTACH inherits the *connection's*
	// VFS unless the target URI names one. So converting away from an encrypted
	// database with a bare 'file:out.db' target opens the output through
	// adiantum and hands it no key — the failure is at the far end of a full
	// database copy, and the reverse mistake writes a file that looks fine and
	// cannot be read. Both targets therefore name their VFS explicitly.
	if _, err := src.db.ExecContext(ctx, "VACUUM INTO "+sqlite3.Quote(target)); err != nil {
		os.Remove(destination)
		return fmt.Errorf("convert: vacuum into %s: %w", destination, err)
	}

	if err := verifyConversion(ctx, src, destination, destinationKey); err != nil {
		os.Remove(destination)
		return err
	}
	return flushDurably(destination)
}

// vacuumTarget builds the URI VACUUM INTO writes to.
//
// This is the one place the key is allowed into a URI. adiantum takes it from a
// PRAGMA everywhere else, because a DSN reaches error strings — but ATTACH has
// no connection to run a PRAGMA on, so there is nowhere else to put it. The
// exposure is bounded: one short-lived, single-purpose process, and the value
// never leaves this function except into SQLite.
func vacuumTarget(destination string, key []byte) (string, error) {
	if len(key) != 0 && len(key) != KeyLen {
		return "", fmt.Errorf("convert: destination key is %d bytes, want %d", len(key), KeyLen)
	}
	target := &url.URL{Scheme: "file", Path: destination}
	query := url.Values{}
	if len(key) == 0 {
		// Naming the default VFS is not redundant: without it an encrypted
		// source's connection would hand its own adiantum VFS to the ATTACH.
		query.Set("vfs", "os")
	} else {
		query.Set("vfs", "adiantum")
		query.Set("hexkey", hex.EncodeToString(key))
	}
	target.RawQuery = query.Encode()
	return target.String(), nil
}

// verifyConversion reads the result back through its own key and checks it
// against the source.
//
// integrity_check alone would pass on a database that decoded but lost rows, and
// a row count alone would pass on one that is subtly corrupt, so both run.
func verifyConversion(ctx context.Context, src *Store, destination string, key []byte) error {
	dst, err := Open(ctx, destination, key)
	if err != nil {
		return fmt.Errorf("convert: reopen destination: %w", err)
	}
	defer dst.Close()

	var result string
	if err := dst.db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return fmt.Errorf("convert: integrity check: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("convert: integrity check reported %q", result)
	}

	var want, got int64
	if err := src.db.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&want); err != nil {
		return fmt.Errorf("convert: count source events: %w", err)
	}
	if err := dst.db.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&got); err != nil {
		return fmt.Errorf("convert: count converted events: %w", err)
	}
	if want != got {
		return fmt.Errorf("convert: %d events in the source, %d in the conversion", want, got)
	}
	return nil
}

// flushDurably fsyncs a file and the directory holding it.
//
// The directory matters as much as the file: the caller is about to rename this
// over the live database, and a rename recorded before the bytes it points at
// leaves nothing to recover. internal/compress does the same thing for the same
// reason.
func flushDurably(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("convert: open for flush: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("convert: flush %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("convert: close %s: %w", path, err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("convert: open directory for flush: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("convert: flush directory: %w", err)
	}
	return nil
}
