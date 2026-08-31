package mcpsetup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The snippet is what a user pastes when Lumi declines to write the file, so it
// has to be valid TOML for paths the real world produces — the default data
// directory alone contains a space.
func TestTOMLSnippet(t *testing.T) {
	t.Parallel()
	got := TOMLSnippet(Spec{
		Name:    "lumi",
		Command: "/usr/local/bin/lumi",
		Args:    []string{"mcp", "--data-dir", "/Users/x/Library/Application Support/Lumi"},
	})
	want := "[mcp_servers.lumi]\n" +
		`command = "/usr/local/bin/lumi"` + "\n" +
		`args = ["mcp", "--data-dir", "/Users/x/Library/Application Support/Lumi"]`
	if got != want {
		t.Errorf("TOMLSnippet:\n%s\nwant:\n%s", got, want)
	}
}

// A name or path TOML cannot take bare has to come back quoted, not pasted in
// as-is where it would parse as a different table or fail outright.
func TestTOMLSnippetQuotesWhatItMust(t *testing.T) {
	t.Parallel()
	got := TOMLSnippet(Spec{
		Name:    "lumi work",
		Command: `/opt/we"ird\path/lumi`,
		Args:    []string{"mcp"},
	})
	if !strings.HasPrefix(got, `[mcp_servers."lumi work"]`) {
		t.Errorf("a name needing quotes was written bare:\n%s", got)
	}
	if !strings.Contains(got, `command = "/opt/we\"ird\\path/lumi"`) {
		t.Errorf("a path containing a quote and a backslash was not escaped:\n%s", got)
	}
}

// An env block survives into the snippet, sorted, or a user restoring by hand
// would silently lose it.
func TestTOMLSnippetRendersEnvSorted(t *testing.T) {
	t.Parallel()
	got := entryTOMLSnippet("lumi", entry{
		Command: "/abs/lumi",
		Env:     map[string]string{"B": "2", "A": "1"},
	})
	if !strings.Contains(got, `env = { A = "1", B = "2" }`) {
		t.Errorf("env is missing or unsorted:\n%s", got)
	}
	// No args line at all rather than an empty one: `args = []` is a different
	// statement from saying nothing.
	if strings.Contains(got, "args") {
		t.Errorf("an entry with no args rendered an args key:\n%s", got)
	}
}

// The two paste-by-hand instructions must always match the syntax they
// introduce, which is why they are set together.
func TestManualHintMatchesTheFormat(t *testing.T) {
	t.Parallel()
	spec := testSpec()

	var jsonResult Result
	jsonResult.manualJSON(spec)
	if jsonResult.ManualHint != jsonManualHint || !strings.HasPrefix(jsonResult.Manual, `  "lumi"`) {
		t.Errorf("manualJSON = %+v, want a JSON fragment under mcpServers", jsonResult)
	}

	var tomlResult Result
	tomlResult.manualTOML(spec)
	if tomlResult.ManualHint != tomlManualHint || !strings.HasPrefix(tomlResult.Manual, "[mcp_servers.") {
		t.Errorf("manualTOML = %+v, want a TOML table", tomlResult)
	}
}

// TestMain stubs the shell probe for every test in this package. A unit test
// must never spawn a real interactive shell, and the developer's own installs
// must not answer for the machine under test.
func TestMain(m *testing.M) {
	userPATH = func() string { return "" }
	os.Exit(m.Run())
}

func TestParsePATHProbe(t *testing.T) {
	const marked = "\n" + pathMarker + "/usr/bin:/bin\n"
	tests := map[string]struct {
		out  string
		want string
	}{
		"clean": {marked, "/usr/bin:/bin"},
		// A startup banner prints before the -c command runs.
		"banner with newline":    {"welcome\n" + marked, "/usr/bin:/bin"},
		"banner without newline": {"welcome" + marked, "/usr/bin:/bin"},
		// ~/.zlogout runs *after* it, with nothing between the two. Measured:
		// without the marker this appended itself to the last PATH entry.
		"logout message glued on":   {"\n" + pathMarker + "/usr/bin:/bin\ngoodbye, saving to /tmp/x", "/usr/bin:/bin"},
		"logout message no newline": {marked + "bye", "/usr/bin:/bin"},
		// csh and tcsh reject -l; the retry, not the parser, answers for them.
		"shell usage error": {"Unknown option: `-l'\nUsage: csh [ -bcdefilmnqstvVxX ]\n", ""},
		"empty":             {"", ""},
		"no marker":         {"\n/usr/bin:/bin\n", ""},
		"empty path":        {"\n" + pathMarker + "\n", ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := parsePATHProbe(tc.out); got != tc.want {
				t.Errorf("parsePATHProbe(%q) = %q, want %q", tc.out, got, tc.want)
			}
		})
	}
}

// The whole point of the probe: a binary that is not on this process's PATH but
// is on the user's shell's is found.
func TestLookCLIFindsABinaryOnlyTheUserShellKnows(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "somemcpclient")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Not on this process's PATH: exec.LookPath must miss it.
	t.Setenv("PATH", "/nonexistent")
	if _, err := lookCLI("somemcpclient"); err == nil {
		t.Fatal("lookCLI found the binary before the user PATH was in play")
	}

	stubUserPATH(t, dir)

	got, err := lookCLI("somemcpclient")
	if err != nil {
		t.Fatalf("lookCLI: %v", err)
	}
	if got != binary {
		t.Errorf("lookCLI = %q, want %q", got, binary)
	}
}

// An unusable candidate in an earlier directory must not end the search. A
// directory named `codex`, or one owned by somebody else that this user cannot
// execute, would otherwise shadow the real CLI further along the user's PATH.
func TestLookCLIScansPastAnUnusableCandidate(t *testing.T) {
	shadow, real := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(shadow, "adirectory"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shadow, "notexecutable"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "/nonexistent")
	stubUserPATH(t, shadow+string(filepath.ListSeparator)+real)

	for _, name := range []string{"adirectory", "notexecutable"} {
		// Nothing usable anywhere: not found, and no panic on the way.
		if got, err := lookCLI(name); err == nil {
			t.Errorf("lookCLI(%q) = %q, want not found", name, got)
		}
		// Now the same name exists, usable, in the later directory.
		wanted := filepath.Join(real, name)
		if err := os.WriteFile(wanted, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := lookCLI(name)
		if err != nil {
			t.Fatalf("lookCLI(%q) after the shadow: %v", name, err)
		}
		if got != wanted {
			t.Errorf("lookCLI(%q) = %q, want %q", name, got, wanted)
		}
	}
}

// stubUserPATH points the probe at a fixed PATH for one test.
func stubUserPATH(t *testing.T, path string) {
	t.Helper()
	previous := userPATH
	t.Cleanup(func() { userPATH = previous })
	userPATH = func() string { return path }
}
