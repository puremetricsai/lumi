# internal/config

Resolves `Paths` from `--data-dir`, else `LUMI_HOME`, else `~/Library/Application Support/Lumi`; directories
created 0700.

An agent launches `lumi mcp` with a bare environment, so `--data-dir`/`LUMI_HOME` must be the whole story —
nothing may fall back to the current working directory or to state written elsewhere. `Paths.Ensure` is the
only thing that creates directories, and `lumi mcp setup --dry-run` deliberately skips it
(`internal/mcpsetup/CLAUDE.md`).
