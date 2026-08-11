package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/puremetricsai/lumi/internal/compress"
	"github.com/puremetricsai/lumi/internal/config"
)

func (a *app) compressCommand() *cobra.Command {
	var (
		olderThan      string
		screens, audio string
		quality        float64
		minPSNR        float64
		vacuum         bool
		whileRecording bool
		dryRun, asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "compress",
		Short: "Re-encode indexed media in place and reclaim database space",
		Long: "Re-encode the screenshots and audio already on disk into smaller files, leaving every\n" +
			"event exactly where it is. Screenshots become HEIC and audio becomes lossless FLAC,\n" +
			"then the database is rebuilt to return the free pages a prune left behind.\n\n" +
			"Compress and prune are complementary: prune decides what history to keep, compress\n" +
			"decides how densely to keep it. Nothing here deletes an event.\n\n" +
			"--older-than takes a Go duration (48h) or an RFC3339 timestamp; Go durations have no\n" +
			"'d' unit. It defaults to 48h because recompressing what was captured a moment ago\n" +
			"competes with the recorder for files it is still writing.\n\n" +
			"Audio is lossless, so its samples are recoverable bit for bit. HEIC is not: it is a\n" +
			"second lossy generation on top of the capture JPEG and cannot be undone. Use\n" +
			"--screens none to decline it; docs/compress.md records the decision in full.\n\n" +
			"A dry run reports what would be compressed but cannot report a ratio, because\n" +
			"measuring one means doing the encode.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			screenCodec, err := parseScreenCodec(screens)
			if err != nil {
				return err
			}
			audioCodec, err := parseAudioCodec(audio)
			if err != nil {
				return err
			}
			before, err := parseTime(olderThan, true)
			if err != nil {
				return fmt.Errorf("parse --older-than: %w", err)
			}

			paths, err := a.paths()
			if err != nil {
				return err
			}
			// A dry run writes nothing, so neither guard applies to it.
			if !dryRun {
				if !whileRecording {
					if err := refuseCompressWhileRecording(paths); err != nil {
						return err
					}
				}
				release, err := lockCompress(paths)
				if err != nil {
					return err
				}
				defer release()
			}

			s, paths, err := a.openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()

			result, err := compress.Compress(cmd.Context(), s, compress.Options{
				Before:       before,
				Screens:      screenCodec,
				Audio:        audioCodec,
				Quality:      quality,
				MinPSNRDB:    minPSNR,
				Images:       compress.NativeImages{},
				Sounds:       compress.NativeAudio{},
				MediaDirs:    []string{paths.Screenshots, paths.Audio},
				Vacuum:       vacuum,
				DatabasePath: paths.Database,
				DryRun:       dryRun,
				Logger:       slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil)),
			})
			if err != nil {
				return err
			}
			if asJSON {
				encoder := json.NewEncoder(os.Stdout)
				encoder.SetIndent("", "  ")
				return encoder.Encode(result)
			}
			printCompressResult(os.Stdout, result, dryRun)
			return nil
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&olderThan, "older-than", "48h",
		"only compress events older than this duration (e.g. 48h) or RFC3339 time")
	flags.StringVar(&screens, "screens", string(compress.CodecHEIC), "screenshot codec: heic or none")
	flags.StringVar(&audio, "audio", string(compress.CodecFLAC), "audio codec: flac or none")
	flags.Float64Var(&quality, "quality", compress.DefaultQuality, "HEIC quality between 0 and 1")
	flags.Float64Var(&minPSNR, "min-psnr", compress.DefaultMinPSNRDB,
		"reject a re-encoded image below this PSNR in dB and keep the original")
	flags.BoolVar(&vacuum, "vacuum", true, "rebuild the database afterwards to reclaim free pages")
	flags.BoolVar(&whileRecording, "while-recording", false, "run even though the recorder is active")
	flags.BoolVar(&dryRun, "dry-run", false, "report what would be compressed without compressing")
	flags.BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

// parseScreenCodec rejects hevc by name rather than as an unknown value.
//
// The inter-frame video path is the obvious next thing to reach for — it is
// worth another 2.7x — and it is deliberately absent: it needs a schema
// migration, a frame-retrieval API, and reference-counted movie lifetimes,
// because one file would then hold many events. "Invalid value" would read as a
// typo and send someone looking for the right spelling.
func parseScreenCodec(value string) (compress.Codec, error) {
	switch compress.Codec(value) {
	case compress.CodecHEIC, compress.CodecNone:
		return compress.Codec(value), nil
	case "hevc":
		return "", errors.New("--screens hevc is not available yet: encoding runs of frames into one " +
			"movie means many events sharing a file, which needs a schema change and a way to fetch " +
			"a single frame back. Use --screens heic")
	default:
		return "", fmt.Errorf("--screens must be heic or none, not %q", value)
	}
}

func parseAudioCodec(value string) (compress.Codec, error) {
	switch compress.Codec(value) {
	case compress.CodecFLAC, compress.CodecNone:
		return compress.Codec(value), nil
	default:
		return "", fmt.Errorf("--audio must be flac or none, not %q", value)
	}
}

// refuseCompressWhileRecording keeps compress off an index a recorder is
// actively writing.
//
// Unlike the backfill's gate, which fires only for --retranscribe because its
// hazard is compute, this one is unconditional: compress rewrites media_path
// under a live writer that resolves a path and then opens the file, and its
// VACUUM wants an exclusive lock. Neither is a correctness hazard any more —
// reconcile is sibling-scoped and the row update is a compare-and-swap — so this
// is contention avoidance, which is why --while-recording may override it.
func refuseCompressWhileRecording(paths config.Paths) error {
	state, ok, err := readRecordState(paths)
	if err != nil || !ok || !processAlive(state.PID) {
		return nil
	}
	return fmt.Errorf("recording is in progress (pid %d) and compress rewrites the media paths it is "+
		"writing; stop it with `lumi record stop`, or pass --while-recording to override", state.PID)
}

// compressLockName is the file that makes compress single-instance. It lives
// beside the recorder's state file for the same reason: any terminal can find it.
const compressLockName = "compress.lock"

// lockCompress refuses to start while another compress run holds the lock.
//
// This is not politeness about contention, it is what makes the crash-safety
// story true. Destination paths are deterministic, so two runs collide on the
// same file: the first can commit its row and delete the original while the
// second, having lost the compare-and-swap, unlinks the destination — which is
// by then the file the first run's row names. That leaves a row pointing at
// nothing with no original left, the one state the whole ordering exists to
// prevent.
//
// A pid file rather than flock, because internal/cli carries no build tag and
// flock would need a per-platform pair. A stale lock left by a killed process is
// taken over, the same way `record start` treats a dead recorder.
func lockCompress(paths config.Paths) (func(), error) {
	path := filepath.Join(paths.Root, compressLockName)
	if err := os.MkdirAll(paths.Root, 0o700); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(file, "%d %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
			file.Close()
			return func() { os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("take the compress lock: %w", err)
		}
		holder := lockHolder(path)
		if processAlive(holder) {
			return nil, fmt.Errorf("compression is already in progress (pid %d); "+
				"two runs would race for the same files", holder)
		}
		// Nobody holds it — a previous run was killed. Take it over.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("clear the stale compress lock: %w", err)
		}
	}
	return nil, errors.New("could not take the compress lock")
}

// lockHolder reads the pid out of a lock file, returning 0 when it says nothing
// usable — an empty or truncated lock is treated as stale rather than as a live
// holder nobody can identify.
func lockHolder(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var pid int
	if _, err := fmt.Sscanf(string(data), "%d", &pid); err != nil {
		return 0
	}
	return pid
}

func printCompressResult(out io.Writer, result compress.Result, dryRun bool) {
	verb := "compressed"
	if dryRun {
		verb = "would compress"
	}
	printPass(out, verb, "screenshots", result.Screens, dryRun)
	printPass(out, verb, "audio files", result.Audio, dryRun)

	if result.Reconciled.Removed > 0 {
		fmt.Fprintf(out, "cleaned up %d leftover files from an interrupted run, %s\n",
			result.Reconciled.Removed, mib(result.Reconciled.Bytes))
	}
	if result.Reconciled.Recovered > 0 {
		// Worth its own line: it means an earlier run did not finish, and that the
		// media it had already compressed was about to be orphaned.
		fmt.Fprintf(out, "recovered %d events whose media an interrupted run had left unreferenced\n",
			result.Reconciled.Recovered)
	}

	switch result.Vacuum.Status {
	case "done":
		fmt.Fprintf(out, "vacuum: %s → %s\n", mib(result.Vacuum.BytesBefore), mib(result.Vacuum.BytesAfter))
	case "busy":
		fmt.Fprintf(out, "vacuum: skipped, %s\n", result.Vacuum.Detail)
	case "skipped":
		if !dryRun {
			fmt.Fprintf(out, "vacuum: skipped, %s\n", result.Vacuum.Detail)
		}
	}
}

func printPass(out io.Writer, verb, noun string, pass compress.PassResult, dryRun bool) {
	if pass.Files > 0 {
		if dryRun {
			// No ratio: producing one means running the encoder.
			fmt.Fprintf(out, "%s %d %s, %s\n", verb, pass.Files, noun, mib(pass.BytesBefore))
		} else {
			fmt.Fprintf(out, "%s %d %s, %s → %s (%s)\n", verb, pass.Files, noun,
				mib(pass.BytesBefore), mib(pass.BytesAfter), ratio(pass.BytesBefore, pass.BytesAfter))
		}
	}
	if pass.AlreadyDone > 0 {
		fmt.Fprintf(out, "%d %s were already compressed\n", pass.AlreadyDone, noun)
	}
	if pass.MissingFiles > 0 {
		fmt.Fprintf(out, "%d %s referenced media that was already gone\n", pass.MissingFiles, noun)
	}
	// The failure counters print only when they fire. VerifyFailed is the one
	// that matters: it means an encoder produced something wrong while reporting
	// success, and the originals were kept because of it.
	if pass.VerifyFailed > 0 {
		fmt.Fprintf(out, "%d %s failed verification and were left untouched; "+
			"their originals are intact\n", pass.VerifyFailed, noun)
	}
	if pass.EncodeFailed > 0 {
		fmt.Fprintf(out, "%d %s could not be encoded and were left untouched\n", pass.EncodeFailed, noun)
	}
	if pass.Raced > 0 {
		fmt.Fprintf(out, "%d %s were changed by something else mid-run and were skipped\n", pass.Raced, noun)
	}
}

func mib(bytes int64) string {
	return fmt.Sprintf("%.1f MiB", float64(bytes)/(1024*1024))
}

func ratio(before, after int64) string {
	if after <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.1fx", float64(before)/float64(after))
}
