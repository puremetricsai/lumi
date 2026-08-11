# internal/retention

Age- and size-based pruning behind `lumi prune`. Age runs before size; size walks oldest-first.
`Options.All` enumerates every row via `store.AllEvents` (unbounded, so a far-future `captured_at` is never
skipped), deletes rows-before-files, then sweeps `Options.MediaDirs` for orphans. Only `All` sweeps
directories. No background scheduler.

## Invariants

- **Pruning deletes rows before files.** Orphaned files are recoverable; rows pointing at missing media are
  not.
- **Two paths may delete media an event row points at, and their orderings are opposite and each correct.**
  Prune deletes rows before files. `internal/compress` writes and verifies a replacement, flushes it and its
  directory, repoints the row, and only then unlinks the original. Neither is the general rule: what prune
  protects against is a row naming media that no longer exists, and what compress protects against is a row
  naming media that does not exist *yet*. Do not "fix" either to match the other — read
  `internal/compress/CLAUDE.md` before touching this ordering. Keep deletes batched below SQLite's variable
  limit, and keep dry-run accounting equivalent to a real age-then-size run.
- **`--all` sweeps `Paths.Screenshots`/`Paths.Audio` for orphans** — that is what makes the wipe a real
  privacy guarantee. **Age/size policies must never remove orphans.**
- **The general orphan sweep stays here, behind `--all`'s confirmation, and nothing else may grow one.**
  Captured media exists on disk *before* its event row does — the recorder writes a frame, compares it, then
  inserts — so a command that removed any unreferenced file would delete media that was never indexed yet.
  `lumi compress` needed leftover cleanup and deliberately did **not** get this: it reconciles only a file
  whose extension-swapped sibling is named by a row, which fresh capture never has.

The interactive confirmation `--all` requires lives in `internal/cli/CLAUDE.md`.
