# internal/config

Resolves `Paths` from `--data-dir`, else `LUMI_HOME`, else `~/Library/Application Support/Lumi`; directories
created 0700.

An agent launches `lumi mcp` with a bare environment, so `--data-dir`/`LUMI_HOME` must be the whole story —
nothing may fall back to the current working directory or to state written elsewhere. `Paths.Ensure` is the
only thing that creates directories, and `lumi mcp setup --dry-run` deliberately skips it
(`internal/mcpsetup/CLAUDE.md`).

## Encryption touches no path here

The key is a Keychain item, not a file under `Root`, and it is stored per *user* rather than per data
directory — so `--data-dir` and the app's **Choose…** button both keep working, and one key covers
every store this user has. `Paths` gains nothing.

`record.json` and `record.log` stay plaintext, and the exclusion is stated rather than fixed
(`docs/encryption.md`). The log carries application and window names but never extracted screen text
or transcripts, which are logged as a character count; sealing an append-only log with a whole-file
cipher would mean rewriting it on every line.
