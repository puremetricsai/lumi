# internal/vocabulary

Owns the custom vocabulary file's format, cache, and cap, for the same reason `HasSearchableTerms` lives in
`internal/store`: a caller that needs the rule reads it rather than restating it. `Loader.Load()` caches a
successful read against device, inode, mtime, size, **and mode**, but never caches a failure, so a
`chmod`-broken file is retried every call while a routine re-read costs one `stat`. `Snapshot.Terms` is
always usable and `Err` is advisory, mirroring how `capture.ScreenContext` reports a degraded Accessibility
read rather than failing outright. `MaxTerms` (100) caps the list in file order; terms past it are dropped
and counted in `Snapshot.Dropped`, never silently truncated. No native or third-party dependency, so every
rule is testable without permissions or `liblumispeech.a`.

## Invariants

- **A failed vocabulary read is never cached.** `chmod` changes neither size nor mtime, so a stat-keyed
  cache could never observe recovery, and the recorder would transcribe without vocabulary indefinitely
  while `doctor` — a fresh process with a cold cache — read the same file successfully and called it
  healthy. Found as a defect in the design's first draft, before any code existed.
- **`Snapshot.Changed` compares the resulting snapshot, not the stat key.** That is what lets the
  unconditional retry above cost one log line per failure instead of one per chunk.
- **Absence is `Exists`, never `Err`**, because whether a missing file is acceptable is the caller's policy:
  routine for the recorder, fatal for an explicit `lumi transcribe --vocabulary` path. Gating that guard on
  `Err` alone lets a typo'd path silently produce a baseline transcript — the second defect the design's
  adversarial review found, since a silently-successful baseline is precisely the failure this exception
  exists to prevent.
- **`MaxTerms` is a real cap, not hygiene**: contextual biasing is a budget, and an oversized list dilutes
  every term while inviting false substitutions.
