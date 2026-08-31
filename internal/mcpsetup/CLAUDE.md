# internal/mcpsetup

Registers `lumi mcp` with installed MCP clients, backing `lumi mcp setup`. `Spec` carries a name, binary
path, and argv; `internal/cli` supplies all three. It has no native or third-party dependencies. The three
targets are asymmetric because what each client will tell us differs: Claude Code is *read* from
`~/.claude.json` and *written* only via `claude mcp add`/`remove`; Claude Desktop has no CLI and is
read-modify-written in place; Codex is mediated by `codex mcp get --json`/`add`/`remove` in both directions,
so `~/.codex/config.toml` is never touched. Every external command goes through the `Runner` seam and every
path is an injectable field, so tests need no client present.

## Invariants

- **Never hand-write `~/.claude.json`.** It is ~150KB of live Claude Code state rewritten by a running app,
  so a read-modify-write can drop whatever the app wrote in the interim. Lumi may *read* it to detect an
  entry (reading cannot corrupt, and `claude mcp get` can't answer "does this differ"). The only supported
  writers are `claude mcp add --scope user <name> -- <command> [args…]` and `claude mcp remove`. **The `--`
  separator is load-bearing** — without it `claude`'s parser eats `--data-dir` and the server silently
  points at the default index. Because `--force` removes before it adds, a failed `add` triggers a rollback
  re-adding the original on a fresh timeout detached from cancellation; if that also fails, the error
  carries the lost entry as paste-able JSON.
- **Preserve every key in `claude_desktop_config.json` that Lumi did not write.** Decode to
  `map[string]json.RawMessage` at both levels, replace only `mcpServers[<name>]`, write via
  temp-file-plus-rename so a running app can't read a torn file. Invalid JSON is an error, never something
  to repair — the repair would discard the user's preferences. Detection is on the *directory*, and setup
  never creates it.
- **Never hand-write `~/.codex/config.toml`, and never create `~/.codex`.** It *is* a settings file —
  comments, `[features]`, dozens of `[projects."…"]` tables — and no Go TOML encoder preserves comments.
  `codex mcp add`/`remove` are the only supported writers; they normalize the whole `mcp_servers` table
  (dropping a sibling's `args = []`, floating its timeout, sorting its `env`), which is the client's own
  behaviour and not something Lumi can narrow. Reads go through `codex mcp get <name> --json`, the
  structured comparison `claude mcp get` couldn't give. **The `--` separator is load-bearing here too**
  (`TestCodexAddPassesArgsAfterASeparator`). Detection is the `codex` **binary**, never the presence of
  `~/.codex/`, which the ChatGPT desktop app creates whether or not a CLI exists — and which names no
  binary to run `codex mcp add` with, so it could not drive a registration even if it were evidence.
  `/Applications/ChatGPT.app/Contents/Resources/codex` is in the candidate list on purpose: the desktop app
  and the CLI read and write the same `$CODEX_HOME/config.toml`, so registering through either configures
  both, and it is the one candidate that is a real binary rather than a `node` shim.
- **Neither client's CLI is findable from `Lumi.app`'s PATH, and finding one is not the same as being able
  to run it.** launchd gives the app `/usr/bin:/bin:/usr/sbin:/sbin`, so `exec.LookPath` misses every
  `claude` and `codex` installed by npm, nvm, or any version manager, and the MCP settings tab reported
  clients the user plainly had as "not installed". Enumerating install locations cannot fix it — a version
  manager's directory contains the version, so there is no fixed path to list. `userPATH` asks the user's
  own shell instead, once, memoized. The probe has **two** call sites and both are required. `lookCLI` is
  the first, and alone it only moves the failure: an npm-installed `codex` is a `#!/usr/bin/env node`
  script, so a resolved binary still exits 127 under launchd's PATH and the target reports "codex cannot
  read its own configuration" — a lie about what went wrong. `execRunner` is the second, and it is what
  makes the found binary runnable. Both targets reach the probe only through their injectable `LookPath`
  field, so a test that stubs that field cannot be answered by whatever the developer has installed;
  `TestMain` stubs `userPATH` besides, because a unit test must never spawn a shell.
- **Everything the shell prints around the probe's answer is hostile, and three separate things guard
  against it.** The invocation must be **interactive**, because nvm and its kin initialise from `~/.zshrc`,
  which zsh sources only for an interactive shell — a login-only probe was measured returning a PATH
  without the nvm bin directory it was run to find. That is what drags the user's whole startup chain into
  a settings tab, and with it: a banner printed *before* the answer, a `~/.zlogout` printed *after* it with
  no newline in between (measured appending `goodbye…` to the last PATH entry), and a shell that rejects
  the flags outright — `csh` and `tcsh` answer `-l` with "Unknown option". So the answer is **marked**
  (`pathMarker`) and the *last* marked line wins, which is what makes both the banner and the logout
  message unmistakable; a rejected invocation is retried once without `-l`, which is lazier than a table of
  shell names and self-heals for the next shell that dislikes a flag.
- **`cmd.WaitDelay` is what bounds the probe, not the context.** `CommandContext` kills the shell and not
  its descendants, so an rc file that backgrounds anything inheriting stdout leaves `Output()` waiting on a
  pipe that never closes: measured at **60 seconds against a 5-second timeout**, hanging the settings tab.
  The output is complete by then, so it is parsed *before* the error is consulted — discarding it on a
  `WaitDelay` failure would reintroduce exactly the stall the delay exists to cap. Killing the process
  group instead was considered and declined: the orphan is the user's own rc file's background job, it
  exits on its own, and `WaitDelay` already fixes the part that hurt.

- **A disabled Codex entry is a difference, not a match.** An entry with Lumi's exact command and args but
  `enabled = false` is one codex will not launch; comparing only command and args reported `unchanged` while
  the agent silently never saw Lumi. It is a difference, so `--force` fixes it, and `Result.Current` appends
  `(disabled)`. The decoded field is a `*bool` — codex writes no key for an enabled server, so absent and
  `false` must not collapse. It also blocks rollback: `codex mcp add` cannot re-add an entry disabled, so
  `codexEntry.restorable()` refuses and the error carries the raw JSON.
- **Setup never overwrites an entry it did not write.** A differing entry is a conflict that prints current
  against desired and exits non-zero; only `--force` replaces it, and only after a `.lumi-backup`. Silently
  overwriting destroys a hand-tuned entry; warning but exiting zero leaves the agent pointed at the wrong
  index — the worst failure mode. An entry under Lumi's name that does not *decode* is a conflict too, in
  all three targets.
- **`--dry-run` writes nothing at all, including directories.** `runMCPSetup` skips `Paths.Ensure` under it,
  so previewing a mistyped `--data-dir` doesn't create the root. It may still *read*: `codex mcp get` is the
  only way to know what a dry run would do. That command exits 1 both for an unknown name and an unparseable
  config, so a failure is followed by `codex mcp list --json` as a health probe. A read it cannot trust is
  `StatusFailed`, deliberately not `StatusConflict` — nothing is in the way, and offering `--force` would be
  advice that cannot work. `Changed` stays false in every dry run. A dry run is also the only read-only
  status query this package offers — there is no "what is registered?" call, and `lumi mcp setup --dry-run
  --json` is how the macOS app asks.
- **`Result.Manual` and `ManualHint` are filled in on every result, before anything can go wrong.** They
  used to be set only where Lumi declined to write, which made "copy this client's config" a button that
  worked on a skip and did nothing once setup succeeded. The format follows the client, so it is built here
  and never by a caller; a caller rendering its own snippet would tell a Codex user to paste TOML into a
  JSON object.

Why the argv it is handed is absolute — binary path and `--data-dir` alike — is `internal/cli/CLAUDE.md`.
