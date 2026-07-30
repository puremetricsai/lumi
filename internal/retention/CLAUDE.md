# internal/retention

Age- and size-based pruning behind `lumi prune`. Age runs before size; size walks oldest-first.
`Options.All` enumerates every row via `store.AllEvents` (unbounded, so a far-future `captured_at` is never
skipped), deletes rows-before-files, then sweeps `Options.MediaDirs` for orphans. Only `All` sweeps
directories. No background scheduler.

## Invariants

- **Pruning deletes rows before files.** Orphaned files are recoverable; rows pointing at missing media are
  not.
- **`lumi prune` is the only path permitted to delete media.** Keep deletes batched below SQLite's variable
  limit, and keep dry-run accounting equivalent to a real age-then-size run.
- **`--all` sweeps `Paths.Screenshots`/`Paths.Audio` for orphans** — that is what makes the wipe a real
  privacy guarantee. **Age/size policies must never remove orphans.**

The interactive confirmation `--all` requires lives in `internal/cli/CLAUDE.md`.
