package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// appBundleName is what the menu bar app installs as. Resolution is by path
// rather than by bundle identifier on purpose: LaunchServices answers a
// bundle-identifier lookup from its own index, which happily names a copy that
// was moved or deleted, and handing off to a bundle that is no longer there is
// exactly the failure this command exists to report clearly.
const appBundleName = "Lumi.app"

// The app registers these URLs and handles them itself. A URL is used rather
// than `open -a Lumi.app --args …` because LaunchServices drops the arguments
// when the app is already running, which is the common case — the menu bar app
// is meant to be always on. A URL reaches a running app and launches a stopped
// one, with the same call.
const (
	appURLOpen     = "lumi://open"
	appURLSettings = "lumi://settings"
	appURLQuit     = "lumi://quit"
)

// errNoAppBundle is returned when no Lumi.app is installed. `lumi app` is the
// one command whose whole job is to reach the bundle, so this is a failure
// rather than a notice, and the exit code says so.
//
// install.sh is named first because it is how everyone who did not clone the
// repository gets the app, and it installs the bundle where this command looks
// for it. `task app` stays as the developer's path and says so.
var errNoAppBundle = fmt.Errorf(
	"%s is not installed; install it with `curl -fsSL %s | sh`, or build it from a checkout with `task app`",
	appBundleName, installScriptURL)

// openURL and appIsRunning are package vars purely as test seams, for the same
// reason resolveLumiBinary is one: without them a test run would launch or quit
// the developer's own copy of the app.
var (
	openURL      = openURLWithLaunchServices
	appIsRunning = appIsRunningPgrep
	// appInstallRoots is a seam for the same reason. Without it, whether the
	// "no bundle is installed" case can be tested at all depends on whether the
	// developer happens to have Lumi installed — a test that passes on a clean
	// machine and fails on a working one is worse than no test.
	appInstallRoots = defaultAppInstallRoots
)

// defaultAppInstallRoots lists the directories an installed bundle is looked
// for in, in order.
func defaultAppInstallRoots() []string {
	roots := []string{"/Applications"}
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, filepath.Join(home, "Applications"))
	}
	return roots
}

// appCommand hands off to the menu bar app and returns immediately.
//
// Returning immediately is the point, not an optimisation. `lumi` is used in
// pipes, scripts, and shell completions, so a subcommand that blocked until a
// GUI exited would break them. Handing off through LaunchServices also keeps
// the app from becoming a child of the terminal: a GUI spawned from a shell
// dies with the tab, and — the reason that matters here — macOS attributes its
// TCC permissions to the terminal rather than to the bundle.
func (a *app) appCommand() *cobra.Command {
	var settings, quit bool
	cmd := &cobra.Command{
		Use:     "app",
		Aliases: []string{"open"},
		Short:   "Open the Lumi menu bar app",
		Long: "Open the Lumi menu bar app, or bring it to the front if it is already\n" +
			"running. This never starts a second copy, and never blocks the shell.\n\n" +
			"The app supervises its own recorder. Recording is its normal state, so\n" +
			"opening the app starts capture once permissions allow it, and quitting\n" +
			"stops the recorder it owns — gracefully, the same way `lumi record stop`\n" +
			"does.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if settings && quit {
				return errors.New("--settings and --quit cannot be used together")
			}
			bundle, err := resolveAppBundle()
			if err != nil {
				return err
			}
			if quit {
				// Checked before sending anything. `open` would otherwise
				// launch the app for the sole purpose of telling it to quit.
				running, err := appIsRunning(cmd.Context(), bundle)
				if err != nil {
					return err
				}
				if !running {
					fmt.Fprintln(cmd.OutOrStdout(), "Lumi is not running")
					return nil
				}
				return openURL(cmd.Context(), bundle, appURLQuit)
			}
			if settings {
				return openURL(cmd.Context(), bundle, appURLSettings)
			}
			return openURL(cmd.Context(), bundle, appURLOpen)
		},
	}
	cmd.Flags().BoolVar(&settings, "settings", false, "open the app's Settings window")
	cmd.Flags().BoolVar(&quit, "quit", false, "quit the running app (stops the recorder it owns)")
	return cmd
}

// resolveAppBundle finds the installed bundle, or reports that there is none.
func resolveAppBundle() (string, error) {
	for _, candidate := range appBundleSearchPaths() {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate, nil
		}
	}
	return "", errNoAppBundle
}

// appBundleSearchPaths lists where an installed bundle is looked for, in order.
//
// The bundle holding the running binary comes first and is not a fallback: the
// app ships `lumi` inside itself at Contents/MacOS/lumi, so a `lumi` invoked
// from within a bundle must hand off to *that* bundle. Preferring /Applications
// there would silently drive a different copy than the one the user ran.
func appBundleSearchPaths() []string {
	var candidates []string
	if exe, err := resolveLumiBinary(); err == nil {
		if bundle := enclosingAppBundle(exe); bundle != "" {
			candidates = append(candidates, bundle)
		}
	}
	for _, root := range appInstallRoots() {
		candidates = append(candidates, filepath.Join(root, appBundleName))
	}
	return candidates
}

// enclosingAppBundle reports the .app bundle a path lies inside, or "" when it
// lies outside one.
//
// It matches on the path components rather than on an install location,
// because a bundle is a bundle wherever it sits — /Applications, ~/Applications,
// or a build directory during development. Both callers need exactly this
// question answered: `lumi app` to find the bundle to hand off to, and the
// duplicate-recorder refusal to know whether to name the app.
func enclosingAppBundle(path string) string {
	if path == "" {
		return ""
	}
	dir := filepath.Clean(path)
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
		if strings.HasSuffix(dir, ".app") {
			return dir
		}
	}
}

// openURLWithLaunchServices asks LaunchServices to deliver the URL to the
// bundle that was resolved, and to no other.
//
// `-a <bundle>` is the load-bearing part. Sending a bare URL asks LaunchServices
// which application claims the scheme, and it answers from its own index: with a
// stale or duplicate copy registered, the URL reaches a different bundle than
// the one resolveAppBundle just found and reported on. In the --quit case that
// is worse than untidy — it would confirm one bundle is running and then launch
// a second one purely to deliver a quit request.
//
// `open` returns as soon as the request is accepted, which is what keeps this
// command from blocking the shell.
func openURLWithLaunchServices(ctx context.Context, bundle, url string) error {
	out, err := exec.CommandContext(ctx, "/usr/bin/open", "-a", bundle, url).CombinedOutput()
	if err != nil {
		message := strings.TrimRight(string(out), "\r\n")
		if message != "" {
			message = ": " + message
		}
		return fmt.Errorf("open %s%s (%w)", url, message, err)
	}
	return nil
}

// appExecutableName is the bundle's own executable, which is deliberately not
// "Lumi". The bundle also ships this CLI at Contents/MacOS/lumi, and macOS
// filesystems are case-insensitive by default, so the two would be one file.
const appExecutableName = "LumiApp"

// appExecutable is the bundle's own executable, the path a running copy shows
// as argv[0].
func appExecutable(bundle string) string {
	return filepath.Join(bundle, "Contents", "MacOS", appExecutableName)
}

// pgrepPattern builds the pattern that matches exactly this bundle's executable
// and nothing else.
//
// `pgrep -f` takes an extended regular expression, not a literal, and matches it
// anywhere in the full command line. Both halves of that need answering. The
// path is quoted because it always contains at least one metacharacter — the dot
// in "Lumi.app" — and a home directory or build path may contain more, so an
// unquoted path is a pattern that matches more than it names. It is anchored
// because an unanchored match also finds every process that merely *mentions*
// the path in its arguments, and a false positive here sends a quit request to
// an app that is not running.
func pgrepPattern(bundle string) string {
	return "^" + regexp.QuoteMeta(appExecutable(bundle))
}

// appIsRunningPgrep reports whether the bundle's own executable has a live
// process. It matches the executable path rather than the process name so a
// second bundle somewhere else on disk is not mistaken for this one.
func appIsRunningPgrep(ctx context.Context, bundle string) (bool, error) {
	err := exec.CommandContext(ctx, "/usr/bin/pgrep", "-f", pgrepPattern(bundle)).Run()
	if err == nil {
		return true, nil
	}
	// pgrep exits 1 for "no process matched", which is an answer rather than a
	// failure. Anything else is a failure to ask the question at all.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("check whether %s is running: %w", appBundleName, err)
}
