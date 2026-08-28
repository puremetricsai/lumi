# Development

Lumi is a Swift menu-bar app wrapped around a Go binary that does all the work. The app supervises that binary as a child process; the binary is embedded in the bundle at `Contents/MacOS/lumi` and is not installed separately.

```sh
task build   # compiles the Swift SpeechAnalyzer bridge, then go build
task test    # full suite (raw go build/go test will not link without the Swift archive)
task check   # fmt → vet → test; the verification command
```

```sh
task app          # build build/Lumi.app
task app:install  # install ~/Applications/Lumi.app
task app:run      # install and launch it
```

`./scripts/restart-lumi-app.sh` is the app development loop: quit, rebuild, reset TCC, relaunch. A bundle built locally is ad-hoc signed, so every rebuild changes its TCC identity and the four permissions have to be granted again — batch UI changes into single builds. The released app carries one stable certificate, so its grants survive an upgrade.

The binary is also how the pipeline is driven directly while working on it:

```sh
./lumi permissions --request   # or: task permissions
./lumi doctor                  # platform, permissions, speech assets, data directory
./lumi record start --foreground --no-audio --duration 10s   # bounded smoke test
./lumi search "quarterly roadmap"
./lumi search "launch budget" --type audio --since 8h
./lumi transcript --since 2h
./lumi transcript backfill --since 7d
```

`search` returns audio as 30-second windows of one track, which reads poorly as conversation. `transcript` reads the same audio as an ordered conversation instead, labelling every turn by where the sound came from and showing the machine's words once rather than twice. A leading `~` marks a turn whose position was inferred; a trailing score marks uncertain attribution. Turns are derived from the two transcripts, so `transcript backfill` is what fills them in for audio captured before that existed — the default pass works from the index alone, while `--retranscribe` re-runs recognition to recover word timings and refuses to run beside a live recorder.

Two more smoke tests, both permission-gated:

```sh
task test:native   # bounded native framework test
task mcp           # hand-fed MCP JSON-RPC handshake
```
