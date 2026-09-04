# internal/cli

Cobra commands (`record start`/`status`/`stop`, `search`, `mcp`, `prune`, `compress`, `doctor`,
`permissions`, `native-smoke`, `transcribe`, `transcript`, `version`, `app`, `update`). This package wires concrete processors into a
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
  deliberately does not `EvalSymlinks`: resolving can only turn a stable name into an unstable one. A
  packaged install is `/Applications/Lumi.app/Contents/MacOS/lumi`, which `install.sh` replaces along with
  the bundle around it — same path, new file — and a symlinked one keeps the link's own name while its
  target moves every version bump.
  Because the comparison is exact, an entry written against any other binary path no longer equals the
  desired argv, so every client holding one reports `conflict`. `--force`, scoped by `--client`, is the
  way through.
- **`--client` accepts a `Target`'s own name as well as the short one.** A caller reading the JSON has only
  the target name, so accepting it is what lets `Lumi.app` replace one conflicting entry by handing back the
  `target` it was given rather than keeping a second copy of this vocabulary in Swift.
  `TestEveryTargetNameIsAClientValue` derives the check from `defaultSetupTargets`, so a fourth client fails
  it until `parseClientSelection` learns both of its names.
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
- **`lumi update` is the only outbound network call this binary makes**, besides Apple's on-device
  speech-asset download, and it sends nothing but a bare `GET` — no query, no token, no identifier.
  That is the single place Lumi's local-first promise is spent, so anything added here spends it
  again. **It resolves the `latest` redirect rather than the GitHub API**: that is the same pointer
  `install.sh` follows to download the asset, so the check and the install cannot disagree about which
  release is newest, and there is no rate limit or token to hold. The redirect is deliberately not
  followed — its target *is* the answer — and `latest`, `.`, and `/` are rejected as tags, because a
  repository with no published release redirects to itself and comparing "latest" as a version would
  be a silent wrong answer.
- **Both version strings are normalized to a leading `v` before any `semver` call, and a development
  build never checks at all.** `semver.IsValid("0.3.0")` is *false*, and both unprefixed forms are
  real: the release stamps the tag verbatim (`v0.3.0`) while `root.go`'s default is `0.1.0-dev`.
  Skipping the normalization fails silently rather than loudly — an invalid string sorts below every
  valid one, so a released build would offer an update to its own version. The dev build returns
  before the request is made, not after the answer is ignored: `install.sh` does not manage a
  checkout, so no response could change the outcome, and every `task build` would otherwise both nag
  and phone home.
- **`update --apply` refuses before it downloads or quits anything, and the two refusals are there
  for different reasons.** The running binary must be inside `/Applications/Lumi.app`
  (`enclosingAppBundle`, the same helper `lumi app` uses) — a check `install.sh` does not make at
  all, because it replaces that path whatever invoked it, so a copy running from `~/Applications`
  would silently upgrade a *different* Lumi and report success. `/Applications` must be writable —
  which `install.sh` does check, first thing, but its answer there is to re-run under `sudo`, and it
  is by then a detached shell writing into a log nobody reads, naming a command the app cannot run. **Neither refusal may name a
  command**: `Lumi.app` surfaces this text verbatim and there is no `lumi` on anyone's `PATH`
  (`macos/CLAUDE.md`). It then hands off to `install.sh` through `startDetachedProcess` with
  `Setsid`, logging to `paths.UpdateLog`, and returns — not `CommandContext`, whose cancellation on
  return would kill the installer mid-download. Being orphaned to launchd is what lets it outlive both
  the app quitting and this process's own bundle being replaced.
- **The binary refuses to run on anything but `darwin/arm64`** (`platform.Validate` in `PersistentPreRunE`).

## Encryption

- **The Keychain is the authority on whether encryption is on; the file headers only say how far a
  conversion got.** Reading intent off a header breaks the case that matters most: turn encryption on
  before anything is recorded and there is no `lumi.db` at all, so a header check reads "plaintext",
  `openStore` creates a plaintext database, and the next keyed writer fails with "file is not a
  database" on a store the user believes was encrypted from the first frame. `encryptionEnabled` asks
  the Keychain, for attributes only, so refusing a command costs nobody a prompt. The two disagreeing
  is a half-finished conversion — `lumi doctor` reports it and `lumi encrypt` resumes it.
- **The content guard is a cobra annotation whose default is to refuse.** A command that declares
  nothing does not run. A new content-emitting command therefore fails closed on its author's machine
  rather than leaking on a user's, and `TestEveryCommandDeclaresItsContent` — walking the tree from
  `newRootCommand` — is the enforcement, not code review.
- **An unreadable Keychain refuses rather than guesses.** Treating "could not ask" as "encryption is
  off" would print the whole index on exactly the machine where something is already wrong.
- **It is a speed bump, not a boundary, and no wording here may say otherwise.** `lumi mcp` reads the
  key without prompting, so anything that can spawn it can drive JSON-RPC by hand. What the guard buys
  is that ambient access reaches ciphertext and that reading the history takes going through the MCP
  surface the user granted on purpose. `docs/encryption.md` is the honest statement.
- **`lumi encrypt`'s ordering is asymmetric on purpose.** On: store the key, seal the media, convert
  the database. Off: convert the database, unseal the media, delete the key. The irreversible step
  goes where a crash cannot strand anything — the key is written before the first file needs it and
  deleted after the last file stops needing it. And the database goes last on the way in because
  `encrypt status` reads its header, so converting it first would report a finished job while months
  of media were still plaintext.
- **There is no journal and no progress file; the headers are the resume state.** A run killed halfway
  leaves a directory every reader handles correctly, and re-running finishes it. `ensureKey` reuses a
  stored key rather than minting one, because a fresh key on the retry would make everything the first
  run sealed permanently unreadable while reporting success.
- **Clearing `-wal` and `-shm` before the rename is not tidiness.** The write-ahead log holds pages of
  the old database in the old form, so a plaintext `-wal` beside an encrypted `lumi.db` is both a leak
  and a corrupt pair, and neither is visible from the file that was replaced.
- **`lumi reveal` is a second content exit, deliberately.** Media that can never be looked at cannot be
  audited. Its plaintext copy is 0600 in `$TMPDIR` and lives only as long as the QuickLook panel —
  `qlmanage -p` blocks for exactly that, which `open -W` does not, since Preview is usually already
  running and it would return before the file was drawn.
