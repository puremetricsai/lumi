package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// newUpdateTest points the release lookup at a local server that redirects to
// tag, stamps this build's version, and returns a runner for `lumi update`.
// Nothing here reaches the network.
func newUpdateTest(t *testing.T, current, tag string) func(args ...string) (string, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/puremetricsai/lumi/releases/tag/"+tag, http.StatusFound)
	}))
	t.Cleanup(server.Close)
	swap(t, &releasesLatestURL, server.URL)
	swap(t, &version, current)

	a := &app{dataDir: t.TempDir()}
	return func(args ...string) (string, error) {
		cmd := a.updateCommand()
		var stdout bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		err := cmd.ExecuteContext(context.Background())
		return stdout.String(), err
	}
}

func decodeStatus(t *testing.T, out string) updateStatus {
	t.Helper()
	var status updateStatus
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	return status
}

// The version strings this really sees are the release tag (`v0.3.0`) and the
// unprefixed dev default (`0.1.0-dev`), so the unprefixed form is not a
// hypothetical -- omitting the normalization would make `0.3.0` compare as
// invalid, which sorts below every real tag and reports an update against the
// build's own version.
func TestUpdateComparesVersions(t *testing.T) {
	for _, c := range []struct {
		name      string
		current   string
		tag       string
		available bool
		reason    string
	}{
		{name: "newer release", current: "v0.3.0", tag: "v0.4.0", available: true},
		{name: "same release", current: "v0.3.0", tag: "v0.3.0", reason: "no release is newer"},
		{name: "older release", current: "v0.4.0", tag: "v0.3.0", reason: "no release is newer"},
		{name: "unprefixed current", current: "0.3.0", tag: "v0.4.0", available: true},
		{name: "unprefixed tag", current: "v0.3.0", tag: "0.4.0", available: true},
		// A string compare gets this one wrong: "0.10.0" < "0.9.0".
		{name: "double digit minor", current: "v0.9.0", tag: "v0.10.0", available: true},
		{name: "dev build", current: "0.1.0-dev", tag: "v9.9.9", reason: "development build"},
		{name: "prefixed dev build", current: "v0.1.0-dev", tag: "v9.9.9", reason: "development build"},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := newUpdateTest(t, c.current, c.tag)
			out, err := run("--json")
			if err != nil {
				t.Fatal(err)
			}
			status := decodeStatus(t, out)
			if status.UpdateAvailable != c.available {
				t.Fatalf("update_available = %v, want %v (%s)", status.UpdateAvailable, c.available, out)
			}
			if status.Current != c.current {
				t.Errorf("current = %q, want the build's own version %q", status.Current, c.current)
			}
			if c.available && status.Reason != "" {
				t.Errorf("an offered update needs no reason, got %q", status.Reason)
			}
			if !c.available && !strings.Contains(status.Reason, c.reason) {
				t.Errorf("reason = %q, want it to mention %q", status.Reason, c.reason)
			}
			// `latest` is what tells a reader a comparison actually happened.
			// A development build must leave it empty, or the About tab shows a
			// green "up to date" for a version nothing was compared against.
			wantLatest := c.tag
			if strings.Contains(c.reason, "development build") {
				wantLatest = ""
			}
			if status.Latest != wantLatest {
				t.Errorf("latest = %q, want %q", status.Latest, wantLatest)
			}
		})
	}
}

// A tag this build cannot parse is a failed check, not an answer of "no
// update". Returning it as the latter would put a green "up to date" in front of
// somebody whose version was never compared against anything.
func TestUpdateRejectsAnUncomparableTag(t *testing.T) {
	run := newUpdateTest(t, "v0.3.0", "nightly")
	out, err := run("--json")
	if err == nil {
		t.Fatalf("a %q tag was reported as a status: %s", "nightly", out)
	}
	if !strings.Contains(err.Error(), "not a version this build can compare") {
		t.Errorf("error = %q, want it to say the tag could not be compared", err)
	}
}

// A development build must not make the request at all. The answer cannot
// depend on the response, so the call would be one no outcome could change --
// and every developer running this in a checkout would make it.
func TestUpdateDevBuildMakesNoRequest(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Redirect(w, r, "/puremetricsai/lumi/releases/tag/v9.9.9", http.StatusFound)
	}))
	t.Cleanup(server.Close)
	swap(t, &releasesLatestURL, server.URL)
	swap(t, &version, "0.1.0-dev")

	if _, err := checkForUpdate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatalf("a development build made %d requests, want 0", requests)
	}
}

// The redirect target is the answer, so it must not be followed: following it
// downloads a release page to learn what its own URL already said.
func TestUpdateDoesNotFollowTheRedirect(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		http.Redirect(w, r, "/puremetricsai/lumi/releases/tag/v0.4.0", http.StatusFound)
	}))
	t.Cleanup(server.Close)
	swap(t, &releasesLatestURL, server.URL+"/releases/latest")
	swap(t, &version, "v0.3.0")

	status, err := checkForUpdate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Latest != "v0.4.0" {
		t.Fatalf("latest = %q, want it read out of the Location header", status.Latest)
	}
	if len(paths) != 1 {
		t.Fatalf("made %d requests (%v), want exactly the one that is not followed", len(paths), paths)
	}
}

// A repository with no published release redirects back to /releases/latest.
// "latest" is not a tag, and comparing it as one would be a silent wrong answer.
func TestUpdateRejectsANonRelease(t *testing.T) {
	for _, target := range []string{"/releases/latest", "/", ""} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if target == "" {
				w.WriteHeader(http.StatusOK)
				return
			}
			http.Redirect(w, r, target, http.StatusFound)
		}))
		swap(t, &releasesLatestURL, server.URL)
		swap(t, &version, "v0.3.0")
		_, err := checkForUpdate(context.Background())
		server.Close()
		if err == nil {
			t.Fatalf("redirect to %q was accepted as a release", target)
		}
	}
}

// Both refusals must fire before anything is downloaded or quit: reaching
// install.sh with either condition true means the user has already watched Lumi
// stop recording and quit for an upgrade that was never going to land.
func TestUpdateApplyRefusesOutsideTheInstalledBundle(t *testing.T) {
	for _, c := range []struct{ name, exe, want string }{
		{"a checkout binary", filepath.Join(t.TempDir(), "lumi"), "only the copy installed at"},
		{"another bundle", filepath.Join(t.TempDir(), "Lumi.app", "Contents", "MacOS", "lumi"), "only the copy installed at"},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := newUpdateTest(t, "v0.3.0", "v0.4.0")
			swap(t, &resolveLumiBinary, func() (string, error) { return c.exe, nil })
			out, err := run("--apply")
			if err == nil {
				t.Fatalf("--apply succeeded from %s", c.exe)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to mention %q", err, c.want)
			}
			// The refusal must not name a command: Lumi.app surfaces this text
			// and there is no `lumi` on anyone's PATH to run (macos/CLAUDE.md).
			for _, forbidden := range []string{"sudo", "install.sh", "re-run"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Errorf("refusal names %q, which the app cannot ask a user to do: %q", forbidden, err)
				}
			}
			if out != "" {
				t.Errorf("a refused --apply wrote %q to stdout", out)
			}
		})
	}
}

// The unwritable-/Applications refusal has no test of its own: asserting it
// would mean chmod'ing the real /Applications, and asserting the opposite makes
// the result depend on whether the developer's own machine happens to be locked
// down. The bundle refusals above already pin the property that matters -- both
// guards return before any shell is spawned.
func TestUpdateApplyRejectsJSON(t *testing.T) {
	run := newUpdateTest(t, "v0.3.0", "v0.4.0")
	if _, err := run("--apply", "--json"); err == nil {
		t.Fatal("--apply --json was accepted; the flags describe different outputs")
	}
}
