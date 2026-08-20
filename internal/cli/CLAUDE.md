# internal/cli

Cobra commands (`record start`/`status`/`stop`, `search`, `mcp`, `prune`, `compress`, `doctor`,
`permissions`, `native-smoke`, `transcribe`, `transcript`, `version`, `app`). This package wires concrete processors into a
`capture.Recorder`; data flows one way from here and never back. `record start` detaches to the background
by default (`--foreground` keeps it inline) as a re-exec tracked by a JSON state file and log under the data
dir (`record_daemon.go`); `record stop` sends SIGTERM and waits for graceful shutdown. `search` offers exact
case-insensitive app filtering, case-insensitive window-substring filtering, and `--type all|screen|audio`;
`--json` is a bare `[]store.Event` export and both tracks of an audio chunk stay separate rows. `transcript` keeps its own `RunE` for the same reason `mcp` does: reading a transcript
is the useful thing, and `backfill` is maintenance hanging off it. `permissions --request` invokes native
TCC flows — never add `tccutil reset` as a side effect.

`mcp` opens the store through the same `openStore` as every other command (an agent launches it with a bare
environment, so `--data-dir`/`LUMI_HOME` must be the whole story) and treats a cancelled context as a clean
exit. It keeps its own `RunE` rather than becoming a bare parent that prints help — this command *is* the
server, and a help dump would land on the JSON-RPC stream. `mcp setup` is its only subcommand; there is
deliberately no `mcp start`, HTTP transport, or daemon. `resolveLumiBinary`, `verifyLumiBinary`, and
`newSetupTargets` are package vars purely as test seams — without them a test run would rewrite the
developer's own Claude config.

## Invariants

- **Nothing in the `lumi mcp` path may write to stdout except JSON-RPC frames.** `mcpCommand` sets
  `SilenceUsage`/`SilenceErrors` explicitly and every diagnostic goes to stderr — see
  `internal/mcp/CLAUDE.md` for what a stray write costs. The `slog.Logger` it hands `mcp.Serve` is built on
  `cmd.ErrOrStderr()` for that reason; a default handler would corrupt the stream.
- **`mcp` wires the binary watcher, and a watcher it cannot build is a warning rather than a failure.** The
  client owning this process's lifecycle is also why the process watches its own binary: it is launched once
  and kept for the session, so an upgrade otherwise never reaches the running server and the client will not
  relaunch it to fix that. `internal/mcp` takes the hooks rather than doing this itself because that package
  touches no filesystem; `internal/selfexec` owns the rules about which path to watch. Losing the watcher
  costs the upgrade path, not the server, so it must never abort startup.
- **`doctor` never opens the store through `openStore`**, which would create a mistyped `--data-dir` and
  then call the empty result healthy. It reports observed attribution from the index alongside permission
  status.
- **`prune --all` is irreversible and requires an interactive `yes`** (`confirmPruneAll`); only `--yes` or
  `--dry-run` may skip it. Keep dry-run accounting equivalent to a real age-then-size run. The deletion
  ordering and orphan rules are `internal/retention/CLAUDE.md`.
- **`lumi compress` is single-instance, and the lock is a correctness requirement rather than politeness.**
  Destination paths are deterministic, so two runs collide on one file: the first commits its row and
  deletes the original while the second, having lost the compare-and-swap, unlinks the destination — which
  is by then the file the first run's row names. That is the one unrecoverable state
  `internal/compress`'s ordering exists to prevent, and its crash-safety claim is only true with this lock
  in place. It is `flock`, behind the `unix`/`!unix` pair in `compress_lock_{unix,other}.go`, and the
  non-Unix stub fails closed. A pid file cannot do this job: it must be created and then written, so a
  contender reading it in between sees a file it cannot attribute to anybody, concludes the lock is stale,
  and takes a second one — and the stale-takeover path needed to recover from a killed run is itself a
  check-then-remove race. flock has no stale state to reason about, because the kernel drops the lock when
  the holder dies however abruptly: a lock that exists is held by a process that exists.
  **The lock file is deliberately never unlinked**, and that is not tidiness left undone. The lock is the
  inode, not the name; unlinking on release frees the name and the inode's lock separately, so a contender
  holding an already-opened fd acquires the old inode while a third process creates a fresh one at the same
  path — two holders, the state this lock exists to make impossible. A leftover file carries no lock and
  excludes nobody (`TestCompressIgnoresALockFileNobodyHolds`), so the pid inside it is expected to be stale.
- **`compress`'s recorder gate is unconditional, unlike the backfill's.** The backfill gates only
  `--retranscribe` because its hazard is compute. Compress rewrites `media_path` under a live writer that
  resolves a path and then opens the file. It is contention rather than correctness — reconcile is
  sibling-scoped and the update is a CAS — which is what makes `--while-recording` defensible. A dry run
  writes nothing, so neither guard applies to it. `--older-than` defaults to `48h` rather than empty, and
  `--screens hevc` is rejected *by name* with an explanation instead of as an unknown value, so it does not
  read as a typo. Everything about ordering and deletion is `internal/compress/CLAUDE.md`.
- **`transcript`/`get_transcript` must offer `ResumeFrom`, never `CoveredUntil`** — the two have opposite
  inclusivity (`internal/store/CLAUDE.md`). **Both print local**, like every other time this package renders:
  `ResumeFrom` was UTC while the `CoveredUntil` in the sentence above it was local, so one paragraph named
  two adjacent moments hours apart and read as a gap in the recording. `internal/mcp/CLAUDE.md` has the
  same rule for the wire.
- **The backfill (`attributeStoredChunk`) applies the recorder's rules, it does not restate them.** The
  silent-and-failed gate, the track vocabulary, the energy gate, and the verdict→row conversion are all
  exported from `internal/store`, `internal/transcript`, and `internal/capture` for that reason; see
  `internal/capture/CLAUDE.md`.
- **A re-transcription may contribute timings, never text.** `lumi transcript backfill --retranscribe` runs
  recognition again over the same WAV, under a possibly newer model, and its words may simply differ from
  the ones indexed; installing them puts phrases in a transcript that are absent from `events.text` and from
  the FTS index, so a reader sees a sentence no query can find. Its timings are therefore used only while
  `minRetranscribeSimilarity` says it is saying the same thing — below that the chunk falls back to the text path, which needs no audio at all.
  The `track.Text == ""` skip in `loadTimings` is a cost gate on genuinely silent tracks and nothing more:
  a track whose recognition *failed* never reaches it, because the chunk is declined a step earlier.
- **`lumi mcp setup` bakes an absolute binary path and absolute `--data-dir` into the argv** — always, even
  at the default root. Same bare-environment reason as `lumi mcp`, plus it makes the desired entry a pure
  function of (binary, root), which is what lets the "already configured?" check be an exact comparison. It
  deliberately does not `EvalSymlinks`: a packaged install is reached through a stable symlink whose target
  moves every version bump.
- **`mcp setup --dry-run --json` is the read-only status query `internal/mcpsetup` does not otherwise
  have.** `Target.Apply` is the only entry point and it writes unless `DryRun` is set, so the macOS app's
  MCP tab asks what a run *would* do rather than asking what is registered. `--json` is orthogonal to
  `--dry-run` — the app's Set up button wants the same document back from a real run. The payload names the
  resolved binary and the full argv because `lumiBinaryPath` and the absolute `--data-dir` are precisely
  what a reader cannot rebuild, and it carries `manual`/`manual_hint` on *every* result so the app can
  offer "copy client config" without ever constructing a client's JSON or TOML in Swift.
- **`Recorder.AudioOutputs` and `Recorder.AudioMarkers` are always wired here.** Leaving either nil changes
  what an absent source list *means* in every row written — "no source was found" rather than "no source
  was looked for" — and nothing downstream can tell those apart (`internal/capture/CLAUDE.md`).
- **`lumi transcript` restates the microphone caveat wherever external turns were printed.** The origin
  label alone reads as a speaker to anyone summarising the output, and the WAV it came from is deleted on
  the retention schedule while a summary built from it survives — so a speaker inferred there becomes
  permanent. Any future summary or roll-up command must carry the same marker through.
- **`record.json` is the only capture-ownership mechanism, and `--register-state` is what lets a
  supervisor join it rather than work around it.** Only `startBackground` used to write that file, so a
  foreground recorder was invisible to five things that consult it: the duplicate-start refusal,
  `record status`, `record stop`, `compress`, and `transcript backfill`. `Lumi.app` deliberately holds
  `record start --foreground` as a child — a detached re-exec risks launchd becoming the TCC responsible
  process instead of the bundle — so without registration an app-owned recorder would defeat all five.
  The flag defaults off, which is what keeps every existing `--foreground` caller exactly as it was.
  **Swift never writes or parses `record.json`**: this package owns the `recordState` format, and a second
  copy of it in another language is the drift the root `CLAUDE.md` forbids. The app reads
  `record status --json` instead.
- **Removal of `record.json` is always scoped to a pid** (`removeRecordStateFor`). Two writers now clear
  it — the recorder retiring its own registration on the way out, and `record stop` after the process it
  signalled has gone — and a new recorder may legitimately register between those two moments, because the
  pid it checked is genuinely dead. An unconditional removal at either site deletes the newcomer's
  registration and makes a live recorder invisible, which is the failure registration exists to prevent.
  Neither the scoped removal nor the duplicate check is atomic, and that is accepted: the state file is
  advisory, and a lock would be a second ownership mechanism.
- **`lumi app` resolves a bundle by path and hands off with `open -a <bundle> <url>`.** Never a bare URL:
  LaunchServices answers a scheme from its own index, so a stale or duplicate copy receives the request
  instead of the bundle that was just resolved and reported on — and under `--quit` that would confirm one
  bundle is running and then launch a different one to deliver the quit. The URL scheme is used rather than
  `--args` because LaunchServices drops arguments to an already-running app, which is the normal state for
  a menu bar app. `openURL` and `appIsRunning` are test seams for the same reason `resolveLumiBinary` is
  one: without them a test run would launch or quit the developer's own copy.
- **The CLI refuses to run on anything but `darwin/arm64`** (`platform.Validate` in `PersistentPreRunE`).
