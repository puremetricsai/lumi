# internal/cli

Cobra commands (`record start`/`status`/`stop`, `search`, `mcp`, `prune`, `doctor`, `permissions`,
`native-smoke`, `transcribe`, `transcript`, `version`). This package wires concrete processors into a
`capture.Recorder`; data flows one way from here and never back. `record start` detaches to the background
by default (`--foreground` keeps it inline) as a re-exec tracked by a JSON state file and log under the data
dir (`record_daemon.go`); `record stop` sends SIGTERM and waits for graceful shutdown. `search` offers exact
case-insensitive app filtering, case-insensitive window-substring filtering, `--type all|screen|audio`, and
`--collapse-audio`. `transcript` keeps its own `RunE` for the same reason `mcp` does: reading a transcript
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
  `internal/mcp/CLAUDE.md` for what a stray write costs.
- **`doctor` never opens the store through `openStore`**, which would create a mistyped `--data-dir` and
  then call the empty result healthy. It reports observed attribution from the index alongside permission
  status.
- **`prune --all` is irreversible and requires an interactive `yes`** (`confirmPruneAll`); only `--yes` or
  `--dry-run` may skip it. Keep dry-run accounting equivalent to a real age-then-size run. The deletion
  ordering and orphan rules are `internal/retention/CLAUDE.md`.
- **`transcript`/`get_transcript` must offer `ResumeFrom`, never `CoveredUntil`** — the two have opposite
  inclusivity (`internal/store/CLAUDE.md`).
- **The backfill (`attributeStoredChunk`) applies the recorder's rules, it does not restate them.** The
  silent-and-failed gate, the track vocabulary, the energy gate, and the verdict→row conversion are all
  exported from `internal/store`, `internal/transcript`, and `internal/capture` for that reason; see
  `internal/capture/CLAUDE.md`.
- **A re-transcription may contribute timings, never text.** `lumi transcript backfill --retranscribe` runs
  recognition again over the same WAV, under a possibly newer model, and its words may simply differ from
  the ones indexed; installing them puts phrases in a transcript that are absent from `events.text` and from
  the FTS index, so a reader sees a sentence no query can find. The re-run is therefore biased with the same
  vocabulary the recorder used and its timings are used only while `minRetranscribeSimilarity` says it is
  saying the same thing — below that the chunk falls back to the text path, which needs no audio at all.
  The `track.Text == ""` skip in `loadTimings` is a cost gate on genuinely silent tracks and nothing more:
  a track whose recognition *failed* never reaches it, because the chunk is declined a step earlier.
- **`lumi mcp setup` bakes an absolute binary path and absolute `--data-dir` into the argv** — always, even
  at the default root. Same bare-environment reason as `lumi mcp`, plus it makes the desired entry a pure
  function of (binary, root), which is what lets the "already configured?" check be an exact comparison. It
  deliberately does not `EvalSymlinks`: a packaged install is reached through a stable symlink whose target
  moves every version bump.
- **`Recorder.AudioOutputs` is always wired here.** Leaving it nil changes what an absent
  `active_audio_output_processes` key *means* in every row written (`internal/capture/CLAUDE.md`).
- **The CLI refuses to run on anything but `darwin/arm64`** (`platform.Validate` in `PersistentPreRunE`).
