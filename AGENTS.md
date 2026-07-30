# AGENTS.md

Repository guidance is maintained in [`CLAUDE.md`](./CLAUDE.md). It documents Lumi's current native ScreenCaptureKit/Accessibility/Vision capture and on-device Apple SpeechAnalyzer transcription pipeline, search and retention behavior, schema and media-safety invariants, the Swift-plus-cgo build (`task build`/`task test`, which compile the SpeechAnalyzer bridge before `go build`/`go test`), and verification commands. Read it completely and follow it before making changes in this repository.

Each package under `internal/` carries its own `CLAUDE.md` with the invariants for that package — the root
file is the map and the rules that span packages. Read the package's file too before changing anything in
it.
