# Converting Lumi into a Mac desktop app — architecture research

**Date:** 2026-08-01
**Question:** Lumi should become a Mac desktop application. Should we first convert it completely to Swift and drop Go?
**Answer:** No. Keep the Go core unchanged and put a thin SwiftUI menu-bar shell around it. A full rewrite maximizes regression risk while attacking something that isn't the actual gap.

## Why the rewrite premise doesn't hold

The instinct "a lean native Mac app means all-Swift" rests on an assumption that is false for Lumi specifically:

- **The Apple-platform-risky parts are already Swift/ObjC.** ScreenCaptureKit, Vision OCR, Accessibility, AVFoundation, and SpeechAnalyzer all live in `internal/macosnative` (`native.m`, `speech.swift`) behind cgo. A rewrite would not move Lumi *onto* Apple frameworks — it is already on them. What a rewrite would actually replace is the part Go does well and that the tests protect: the store with its versioned migrations and FTS5 search, transcript attribution rules, the "never lose captured media" invariants, retention, and the MCP server. That is ~27k lines of tested behavior, and `recorder_test.go`'s full-pipeline fake harness does not come along to Swift.
- **What makes Lumi "not a desktop app" today is only the missing shell**: no `.app` bundle identity (so TCC permissions attach to the terminal), no GUI, no login-item lifecycle. That is the constraint — and it is new Swift code *either way*, rewrite or not. The language of the core is irrelevant to it.
- **The two-language cost is already sunk.** The build already requires both the Go and Swift toolchains (`task speech` → `liblumispeech.a`). Adding an Xcode app target adds no toolchain.
- **The architecture is proven at scale.** Tailscale ships its Mac app as Swift UI over a Go core (libtailscale builds it with `-buildmode=c-archive`); Ollama bundles its Go server inside the app. The Go runtime costs a few MB; leanness as users experience it (memory, launch time, native look) is unaffected.

## Recommended architecture

### Phase 1 — Swift shell, zero core changes

A SwiftUI menu-bar app (`LSUIElement`, `MenuBarExtra`) that bundles the existing `lumi` binary inside `Contents/MacOS/` and supervises it. The interfaces the app needs already exist and are tested:

- **Recorder control:** spawn `lumi record start --foreground` as a directly-held child `Process`. Do **not** use the default detaching `record start` from the app — the daemon re-execs and detaches, which risks launchd becoming the TCC "responsible process" instead of the app. A foreground child keeps permission prompts attributed to the `.app` bundle. This fixes today's pain where rebuilding the binary or switching terminals breaks Screen Recording grants — only app bundles get a stable TCC identity.
- **Status:** `record.json` + `lumi record status` already exist.
- **Search UI:** drive `lumi` for queries rather than opening `lumi.db` from Swift (reading the SQLite directly couples the app to the schema that `internal/store/migrations.go` owns). If the CLI's output isn't machine-friendly, add a `--json` flag — small, additive, testable — or speak to `lumi mcp` over stdio, already a stable read-only contract.
- **Packaging:** Developer ID signing + notarization with hardened runtime; the bundled Go binary gets signed and needs the mic/speech entitlements alongside the app. Go binaries notarize fine. `SMAppService` for launch-at-login; Sparkle for updates if wanted. The app cannot be Mac App Store either way — Accessibility-based attribution doesn't survive the sandbox — so Swift purity buys nothing there.
- The CLI keeps working for terminal users (symlink to the bundled binary, as Tailscale does).

### Phase 2 — only if a driver appears

Migrate packages to Swift incrementally behind the existing test surface, or switch subprocess → `c-archive` linkage if the two-process model proves annoying. Default to doing neither: subprocess keeps crash domains separate (a recorder panic can't take down the UI) and reuses the existing graceful-stop logic.

## Spike to run before committing

A day, not a rewrite: build a throwaway signed `.app` that spawns the bundled `lumi record start --foreground` and verify all four TCC prompts (Screen Recording, Microphone, Accessibility, Speech Recognition) attribute to the app bundle and survive an app update. That is the only genuinely uncertain link — everything else is additive UI work on top of a core that stays untouched.

## Alternatives considered and rejected

- **Full Swift rewrite first** — highest regression risk, re-implements the tested behavior surface before shipping any user-visible value, and gains nothing on the Apple-framework side (already native).
- **Wails / Tauri** — WKWebView-based UI (and Rust, for Tauri) contradicts Lumi's lean-native ethos; the app shell is small enough that SwiftUI is less total machinery.
- **`c-archive` embedding from day one** — single process, but couples the app's crash domain and lifecycle to the recorder, complicates signal handling, and discards the already-tested daemon supervision design. Keep as a Phase-2 option.

## Sources

- [libtailscale Swift framework](https://github.com/tailscale/libtailscale/blob/main/swift/README.md)
- [Tailscale on macOS binary architecture](https://tailscale.com/blog/macos-binary-size)
- [macOS screen-recording permissions guide](https://www.screenify.studio/blog/2026-04-23-macos-screen-recording-permissions)
- [TCC internals reference](https://hacktricks.wiki/en/macos-hardening/macos-security-and-privilege-escalation/macos-security-protections/macos-tcc/index.html)
- [Writing Mac apps in Go and Swift](https://youngdynasty.net/posts/writing-mac-apps-in-go/)
