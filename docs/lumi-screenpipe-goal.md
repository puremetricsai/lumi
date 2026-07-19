# Lumi Screenpipe-Parity Goal

Paste the following prompt into a new Codex session from the Lumi repository:

```text
/goal Bring Lumi’s capture and indexing behavior materially closer to screenpipe on Apple Silicon macOS without copying screenpipe code or expanding Lumi into a GUI, plugin platform, HTTP server, or multi-provider application.

Treat the already-landed versioned migrations, app/window search filters, and prune command as the baseline. Replace shell-based capture with a native macOS subsystem built on ScreenCaptureKit. Support all connected displays, display hotplug, and simultaneous system/output and microphone audio. Make the macOS Accessibility tree the primary source of screen text, focused application/window context, and useful UI state. Use Apple Vision text recognition as the screenshot fallback instead of Tesseract.

Add perceptual near-duplicate frame suppression with an activity-aware threshold and a maximum silence interval. Preserve recoverable media whenever downstream extraction or transcription fails. Add permission preflight and actionable diagnostics for Screen Recording, Accessibility, Input Monitoring when required, and Microphone access. Recover automatically from transient stream and device failures.

Keep capture and processing behind testable interfaces. Evolve SQLite through append-only versioned migrations for Accessibility provenance and new display/audio metadata. Preserve FTS5 search compatibility, existing search/ask/retention behavior, and the rule that `lumi ask` sends text and metadata only. Retain whisper.cpp initially. Remove Tesseract and FFmpeg as required runtime capture dependencies once the native implementations reach parity.

Update CLI flags, `doctor`, README, architecture documentation, and automated tests. Add bounded macOS smoke tests and benchmarks demonstrating that native capture works, Accessibility text is preferred, Vision fallback works, system audio is captured, near-duplicates are reduced, transient failures recover, and existing tests remain green. Implement the work in independently testable milestones, running `go test ./...` and `go vet ./...` throughout.
```
