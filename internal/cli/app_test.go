package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// stubLauncher replaces the two seams that would otherwise launch or quit the
// developer's own copy of the app, and returns the URLs the command sent.
func stubLauncher(t *testing.T, running bool) *[]string {
	t.Helper()
	sent := []string{}
	originalOpen, originalRunning := openURL, appIsRunning
	// The bundle is recorded alongside the URL: a handoff that discards the
	// resolved bundle lets LaunchServices pick a different copy, which is
	// exactly what the -a argument exists to prevent.
	openURL = func(_ context.Context, bundle, url string) error {
		sent = append(sent, bundle+" "+url)
		return nil
	}
	appIsRunning = func(context.Context, string) (bool, error) { return running, nil }
	t.Cleanup(func() { openURL, appIsRunning = originalOpen, originalRunning })
	return &sent
}

// installBundle creates a directory that looks like an installed app bundle and
// points binary resolution at the `lumi` inside it, which is how the command
// finds a bundle without touching /Applications.
func installBundle(t *testing.T) string {
	t.Helper()
	bundle := filepath.Join(t.TempDir(), appBundleName)
	if err := os.MkdirAll(filepath.Join(bundle, "Contents", "MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	original := resolveLumiBinary
	resolveLumiBinary = func() (string, error) {
		return filepath.Join(bundle, "Contents", "MacOS", "lumi"), nil
	}
	// An installed copy must never win over the bundle holding the binary, and
	// this makes that assertion independent of whether one is installed here.
	originalRoots := appInstallRoots
	empty := t.TempDir()
	appInstallRoots = func() []string { return []string{empty} }
	t.Cleanup(func() { resolveLumiBinary, appInstallRoots = original, originalRoots })
	return bundle
}

// runAppCommand executes `lumi app` with the given flags.
func runAppCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	a := &app{}
	cmd := a.appCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestEnclosingAppBundle(t *testing.T) {
	for _, c := range []struct{ name, path, want string }{
		{"binary inside a bundle", "/Applications/Lumi.app/Contents/MacOS/lumi", "/Applications/Lumi.app"},
		{"the bundle directory itself is not inside itself", "/Applications/Lumi.app", ""},
		{"outside any bundle", "/usr/local/bin/lumi", ""},
		{"a bundle anywhere on disk", "/Users/x/build/Lumi.app/Contents/MacOS/lumi", "/Users/x/build/Lumi.app"},
		{"nested bundles resolve to the innermost", "/Applications/Outer.app/Contents/Lumi.app/Contents/MacOS/lumi", "/Applications/Outer.app/Contents/Lumi.app"},
		{"empty", "", ""},
		{"root", "/", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := enclosingAppBundle(c.path); got != c.want {
				t.Errorf("enclosingAppBundle(%q) = %q, want %q", c.path, got, c.want)
			}
		})
	}
}

// A lumi shipped inside a bundle must drive that bundle. Preferring an
// installed copy in /Applications would hand off to a different app than the
// one the user actually ran.
func TestResolveAppBundlePrefersTheBundleHoldingTheBinary(t *testing.T) {
	bundle := installBundle(t)
	got, err := resolveAppBundle()
	if err != nil {
		t.Fatalf("resolveAppBundle: %v", err)
	}
	if got != bundle {
		t.Errorf("resolveAppBundle = %q, want the enclosing bundle %q", got, bundle)
	}
}

func TestAppCommandReportsAMissingBundle(t *testing.T) {
	// A binary outside any bundle, and no bundle installed anywhere the search
	// looks — resolution must fail rather than send a URL nothing handles.
	original := resolveLumiBinary
	resolveLumiBinary = func() (string, error) { return filepath.Join(t.TempDir(), "lumi"), nil }
	// Pointed at an empty directory rather than skipping when the developer has
	// Lumi installed: the machine's own state must not decide whether this case
	// is covered.
	originalRoots := appInstallRoots
	empty := t.TempDir()
	appInstallRoots = func() []string { return []string{empty} }
	t.Cleanup(func() { resolveLumiBinary, appInstallRoots = original, originalRoots })
	sent := stubLauncher(t, false)

	_, err := runAppCommand(t)
	if !errors.Is(err, errNoAppBundle) {
		t.Fatalf("error = %v, want errNoAppBundle", err)
	}
	if len(*sent) != 0 {
		t.Errorf("sent %v with no bundle installed; want nothing", *sent)
	}
	// The message is the only thing a user gets here, and it is the one place
	// the CLI names an install command. A README that has moved on from
	// `task app` cannot fix a binary that still tells people to run it.
	for _, want := range []string{"install.sh", "task app"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestAppCommandSendsTheRightURL(t *testing.T) {
	for _, c := range []struct {
		name    string
		args    []string
		running bool
		want    string
	}{
		{"bare opens the window", nil, true, appURLOpen},
		{"--settings opens settings", []string{"--settings"}, true, appURLSettings},
		{"--quit quits a running app", []string{"--quit"}, true, appURLQuit},
	} {
		t.Run(c.name, func(t *testing.T) {
			bundle := installBundle(t)
			sent := stubLauncher(t, c.running)
			if _, err := runAppCommand(t, c.args...); err != nil {
				t.Fatalf("lumi app %v: %v", c.args, err)
			}
			want := bundle + " " + c.want
			if len(*sent) != 1 || (*sent)[0] != want {
				t.Errorf("sent %v, want [%s]", *sent, want)
			}
		})
	}
}

// `--quit` on a stopped app must not launch it just to tell it to quit.
func TestAppQuitDoesNotLaunchAStoppedApp(t *testing.T) {
	installBundle(t)
	sent := stubLauncher(t, false)

	out, err := runAppCommand(t, "--quit")
	if err != nil {
		t.Fatalf("lumi app --quit: %v", err)
	}
	if len(*sent) != 0 {
		t.Errorf("sent %v to a stopped app; want nothing", *sent)
	}
	if !strings.Contains(out, "not running") {
		t.Errorf("output = %q, want a not-running notice", out)
	}
}

func TestAppRejectsSettingsAndQuitTogether(t *testing.T) {
	installBundle(t)
	sent := stubLauncher(t, true)

	if _, err := runAppCommand(t, "--settings", "--quit"); err == nil {
		t.Fatal("--settings --quit was accepted; want an error")
	}
	if len(*sent) != 0 {
		t.Errorf("sent %v for a rejected invocation; want nothing", *sent)
	}
}

// The launcher is reachable as both names; `lumi open` is documented and must
// not quietly stop working.
func TestAppCommandIsAliasedAsOpen(t *testing.T) {
	cmd := (&app{}).appCommand()
	for _, alias := range cmd.Aliases {
		if alias == "open" {
			return
		}
	}
	t.Fatalf("aliases = %v, want them to include \"open\"", cmd.Aliases)
}

// `pgrep -f` takes an extended regular expression and matches it anywhere in the
// command line, so an unquoted, unanchored path is a pattern that matches more
// than it names — and a false positive sends a quit request to an app that is
// not running.
func TestPgrepPatternIsQuotedAndAnchored(t *testing.T) {
	pattern := pgrepPattern("/Users/some.person/Applications/" + appBundleName)
	if !strings.HasPrefix(pattern, "^") {
		t.Errorf("pattern = %q, want it anchored at the start", pattern)
	}
	// Every dot in the path is a metacharacter, and there is always at least
	// one: the dot in "Lumi.app".
	if strings.Contains(pattern, ".app") && !strings.Contains(pattern, `\.app`) {
		t.Errorf("pattern = %q, want the dot in .app quoted", pattern)
	}
	if strings.Contains(pattern, "some.person") {
		t.Errorf("pattern = %q, want the dot in the home directory quoted", pattern)
	}
	// The pattern must still match the real path it was built from.
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("compile %q: %v", pattern, err)
	}
	executable := appExecutable("/Users/some.person/Applications/" + appBundleName)
	if !re.MatchString(executable) {
		t.Errorf("pattern %q does not match its own executable %q", pattern, executable)
	}
	// ...and must not match a path that only differs where a metacharacter was.
	if re.MatchString(appExecutable("/Users/someXperson/Applications/" + appBundleName)) {
		t.Errorf("pattern %q matched a path that differs at the quoted dot", pattern)
	}
}
