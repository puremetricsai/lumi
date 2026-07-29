package mcpsetup

import (
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
