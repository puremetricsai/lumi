package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/puremetricsai/lumi/internal/config"
	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"
)

// installScriptURL is the install command README.md documents, character for
// character. It is fetched from `main` rather than pinned to a tag on purpose:
// the script carries no version and no digest (docs/release.md), so `main` is
// the only place it is ever corrected, and an update taken through a pinned copy
// would run yesterday's installer forever.
const installScriptURL = "https://raw.githubusercontent.com/puremetricsai/lumi/main/install.sh"

// releasesLatestURL redirects to the newest release, which is the same `latest`
// pointer install.sh resolves when it downloads the asset. Reading the tag out
// of that redirect rather than asking the GitHub API is what makes it
// impossible for the check and the install to disagree about which release is
// newest -- they are the same pointer, resolved twice. It also needs no token
// and has no rate limit.
//
// It is a var purely as a test seam, for the same reason `openURL` and
// `resolveLumiBinary` are: without it a test run would reach the network.
var releasesLatestURL = "https://github.com/puremetricsai/lumi/releases/latest"

// installedAppBundle is the one path install.sh replaces. `/Applications` is not
// a free choice -- MCP registration writes the absolute in-bundle path into every
// client's config and TCC keys its grants on path and signature together, so an
// install anywhere else costs users both (docs/release.md).
const installedAppBundle = "/Applications/Lumi.app"

// writeOK is unistd.h's W_OK. package syscall exports the call but not the mode.
const writeOK uint32 = 2

// updateStatus is the whole contract with Lumi.app. Swift decodes exactly this
// and holds no URL, no version comparison, and no knowledge of how an upgrade is
// performed.
type updateStatus struct {
	Current         string `json:"current"`
	Latest          string `json:"latest,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	// Latest is set exactly when a comparison happened, so it -- not Reason --
	// is what separates "no newer release" from "never asked". A development
	// build leaves it empty; a check that could not read the tag is an error
	// rather than a status, so no reader has to parse Reason to tell those
	// apart.
	//
	// Reason says why no update is offered, for a human to read.
	Reason string `json:"reason,omitempty"`
}

// updateCommand checks for a newer release and, with --apply, installs it.
//
// This is the binary's only outbound network call besides Apple's on-device
// speech-asset download, and it sends nothing but the bare GET -- no query, no
// token, no identifier of any kind. `internal/cli/CLAUDE.md` holds that as an
// invariant because it is the one place Lumi's local-first promise is spent.
func (a *app) updateCommand() *cobra.Command {
	var asJSON, apply bool
	cmd := emitsNoContent(&cobra.Command{
		Use:   "update",
		Short: "Check for a newer Lumi release, or install it",
		Long: "Report whether a newer Lumi release exists, and with --apply install it.\n\n" +
			"Detection resolves the same `latest` pointer install.sh downloads from, so\n" +
			"the two can never disagree about which release is newest. --apply runs\n" +
			"install.sh itself rather than reimplementing it: there is one install\n" +
			"channel, and this is not a second one.\n\n" +
			"A build from a checkout always reports no update; install.sh does not\n" +
			"manage it, and no version comparison would make that untrue.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if apply && asJSON {
				return errors.New("--apply and --json cannot be used together")
			}
			if apply {
				paths, err := a.paths()
				if err != nil {
					return err
				}
				log, err := startInstaller(paths)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Installing the latest Lumi. Progress is written to %s\n", log)
				return nil
			}
			status, err := checkForUpdate(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				encoder := json.NewEncoder(cmd.OutOrStdout())
				encoder.SetIndent("", "  ")
				return encoder.Encode(status)
			}
			printUpdateStatus(cmd.OutOrStdout(), status)
			return nil
		},
	})
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	cmd.Flags().BoolVar(&apply, "apply", false, "install the latest release and reopen Lumi")
	return cmd
}

func printUpdateStatus(w io.Writer, status updateStatus) {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintf(tw, "current\t%s\n", status.Current)
	if status.Latest != "" {
		fmt.Fprintf(tw, "latest\t%s\n", status.Latest)
	}
	if status.UpdateAvailable {
		fmt.Fprintf(tw, "update\tavailable\n")
	} else {
		fmt.Fprintf(tw, "update\tnone (%s)\n", status.Reason)
	}
	tw.Flush()
}

// checkForUpdate compares this build against the newest release.
//
// A development build returns before the request is made, not after it is
// ignored. The answer cannot depend on the response, so making the call anyway
// would be a network request that no outcome could change -- and every developer
// running this in a checkout would make it.
func checkForUpdate(ctx context.Context) (updateStatus, error) {
	current := semverTag(version)
	if !semver.IsValid(current) || semver.Prerelease(current) != "" {
		// A checkout was not installed by install.sh, so there is nothing for
		// install.sh to replace and the honest answer is that the question does
		// not apply. Without this every `task build` would nag.
		return updateStatus{Current: version, Reason: "this is a development build, so install.sh does not manage it"}, nil
	}
	latest, err := latestTag(ctx)
	if err != nil {
		return updateStatus{}, err
	}
	// A tag this build cannot parse is a check that failed to answer, not an
	// answer of "no update". Reporting it as the latter would put a green "up to
	// date" in front of somebody whose version was never actually compared.
	newest := semverTag(latest)
	if !semver.IsValid(newest) {
		return updateStatus{}, fmt.Errorf("check for the latest release: it is tagged %q, which is not a version this build can compare against", latest)
	}
	status := updateStatus{Current: version, Latest: latest}
	if semver.Compare(newest, current) > 0 {
		status.UpdateAvailable = true
	} else {
		status.Reason = "no release is newer than the installed version"
	}
	return status, nil
}

// semverTag normalizes a version to the leading "v" that golang.org/x/mod/semver
// requires -- `semver.IsValid("0.3.0")` is false.
//
// Both forms genuinely reach this. A release build is stamped with the git tag
// verbatim, `v0.3.0` (.github/workflows/release-please.yml), and the default in
// root.go is `0.1.0-dev`. Skipping the normalization would not fail loudly: an
// invalid string compares less than every valid one, so a released build would
// report an update against itself, and a caller that tested only the tagged form
// would never see it.
func semverTag(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "v") {
		return s
	}
	return "v" + s
}

// latestTag reads the newest release's tag out of the redirect, without
// following it. The redirect target *is* the answer, so following it would
// download a release page to learn what its own URL already said.
func latestTag(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesLatestURL, nil)
	if err != nil {
		return "", fmt.Errorf("check for the latest release: %w", err)
	}
	client := &http.Client{
		// A menu bar app calls this on a timer, so a hung connection must not
		// become a goroutine that never returns.
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("check for the latest release: %w", err)
	}
	defer resp.Body.Close()

	// The status is checked as well as the header. A Location on a non-redirect
	// carries no promise about where the resource actually is, and treating one
	// as the answer would let a 200 from a captive portal or a misconfigured
	// proxy name a release.
	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		return "", fmt.Errorf("check for the latest release: %s answered HTTP %d instead of redirecting to one", releasesLatestURL, resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if location == "" {
		return "", fmt.Errorf("check for the latest release: %s did not redirect to one (HTTP %d)", releasesLatestURL, resp.StatusCode)
	}
	target, err := url.Parse(location)
	if err != nil {
		return "", fmt.Errorf("check for the latest release: unreadable redirect %q: %w", location, err)
	}
	tag := path.Base(target.Path)
	// "latest" comes back when the repository has no published release at all,
	// and Base answers "." or "/" for a path with no segments. None of those is
	// a tag, and each would otherwise be compared as a version.
	switch tag {
	case ".", "/", "latest":
		return "", fmt.Errorf("check for the latest release: %s names no release yet", releasesLatestURL)
	}
	return tag, nil
}

// startInstaller hands the upgrade to install.sh and returns immediately,
// leaving the shell running after this process -- and the app that spawned it --
// are gone.
//
// Both refusals happen before anything is downloaded or quit. Reaching
// install.sh with either condition true means the user has already watched Lumi
// stop recording and quit for an upgrade that was never going to land.
func startInstaller(paths config.Paths) (string, error) {
	exe, err := resolveLumiBinary()
	if err != nil {
		return "", fmt.Errorf("locate the running Lumi: %w", err)
	}
	// install.sh replaces /Applications/Lumi.app and only that path, so applying
	// from anywhere else -- ~/Applications, a build directory -- would upgrade a
	// different copy than the one the user is looking at.
	if bundle := enclosingAppBundle(exe); bundle != installedAppBundle {
		where := bundle
		if where == "" {
			where = exe
		}
		return "", fmt.Errorf("this Lumi is running from %s; only the copy installed at %s can update itself", where, installedAppBundle)
	}
	// install.sh dies on this check too, but only after downloading and quitting
	// Lumi, and its answer there is to re-run under sudo -- which an app cannot
	// do and must never claim it can.
	installRoot := filepath.Dir(installedAppBundle)
	if err := syscall.Access(installRoot, writeOK); err != nil {
		return "", fmt.Errorf("%s is not writable, so a new Lumi cannot be installed there: %w", installRoot, err)
	}

	logFile, err := os.OpenFile(paths.UpdateLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", fmt.Errorf("open update log: %w", err)
	}
	defer logFile.Close() // the child holds its own descriptor after Start.

	// install.sh never reopens Lumi, so `open -a` is required rather than
	// decorative: without it a user who takes an update is left with no app.
	script := fmt.Sprintf("curl -fsSL %s | sh && open -a '%s'", installScriptURL, installedAppBundle)
	// Deliberately not CommandContext: the context is cancelled the moment this
	// command returns, and that would kill the installer a few milliseconds into
	// its download.
	child := exec.Command("/bin/sh", "-c", script)
	child.Stdin = nil
	child.Stdout = logFile
	child.Stderr = logFile
	// Setsid plus never waiting orphans the shell to launchd. That is what lets
	// it survive the app quitting a moment later, and outlive this process
	// replacing the very bundle it is running from -- install.sh swaps the
	// bundle with mv rather than rm, so nothing ends up executing a deleted
	// image.
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if _, err := startDetachedProcess(child); err != nil {
		return "", fmt.Errorf("start the installer: %w", err)
	}
	return paths.UpdateLog, nil
}
