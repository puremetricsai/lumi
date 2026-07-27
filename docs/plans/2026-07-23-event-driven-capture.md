# Event-Driven Screen Capture

## Summary

Replace the two-second screenshot ticker with a hybrid event coordinator. Lumi will capture an initial baseline, then persist screenshots and focused Accessibility snapshots only for meaningful app, input, visual-motion, display, or audio-activity changes. Continuous audio recording remains unchanged.

## Implementation Changes

- Add a `ScreenTrigger` model containing kind, UTC timestamp, and optional display ID, plus a `ScreenEventSource` consumed by `Recorder`.
- Implement a native activity monitor using:
  - [NSWorkspace notifications](https://developer.apple.com/documentation/appkit/nsworkspace/didactivateapplicationnotification) for app, Space, session, wake, and display changes.
  - Accessibility observers for focused window, focused element, title, value, move, and resize changes.
  - A passive [CGEvent tap](https://developer.apple.com/documentation/coregraphics/cgevent/tapcreate%28tap%3Aplace%3Aoptions%3Aeventsofinterest%3Acallback%3Auserinfo%3A%29) for mouse-down and scroll events only—never keyboard or mouse-move events.
  - One low-resolution, cursor-free ScreenCaptureKit detection stream per display. React to completed frames and ignore idle frames, which explicitly indicate unchanged display content ([SCFrameStatus](https://developer.apple.com/documentation/screencapturekit/scframestatus/idle)).
- Rebuild detection streams after display hotplug. Restart failed monitors with 1s, 2s, then 5s backoff and capture a new baseline after recovery.
- Coalesce trigger bursts in a bounded per-display accumulator:
  - Wait 350ms for UI settling.
  - Enforce a 500ms hard floor between discrete captures.
  - Rate-limit visual motion, scrolling, and continuous Accessibility value changes to one attempt per display every five seconds.
  - Associate clicks and scrolling with the next visual-change event within one second; discard input triggers that produce no visible change.
  - Capture all displays for startup, app/Space, wake, and display-topology events; otherwise capture only the affected display. Audio transitions target the focused display, falling back to all displays when unknown.
- Detect generic video/meeting activity in the existing audio stream using RMS hysteresis: emit `audio_started` after either source exceeds −45 dBFS for 500ms and `audio_stopped` after both remain below −55 dBFS for two seconds. Emit transition events only; ongoing visual motion supplies periodic meeting/video captures.
- Change `ScreenSource.Capture` to accept optional target display IDs. Preserve the existing ScreenCaptureKit JPEG, Vision OCR, Accessibility attribution, and per-display processing pipeline.
- Capture one focused Accessibility snapshot per retained trigger batch. Run Vision only for retained frames.
- Remove `FrameComparer.MaxSilence`; there will be no unchanged heartbeat. Retain a frame when its visual content changes or when a semantic trigger changes app/window/Accessibility context. Audio start/stop events may retain the focused screenshot even when pixels are unchanged.
- Add metadata only—no database migration:
  - `capture_reasons`: coalesced trigger names.
  - `triggered_at`: earliest trigger time in RFC3339Nano UTC.
  - `event_latency_ms`: time from trigger to screenshot.
  Existing provenance columns and search/export behavior remain unchanged.
- Retry a failed screenshot for the same pending change at 1s, 2s, and 5s intervals; newer triggers merge into the retry. Preserve already-written media and diagnostic metadata exactly as today.

## CLI and Permission Behavior

- Keep `--interval` for compatibility, but redefine it as the motion/scroll capture throttle and change its default to five seconds. It no longer starts a polling ticker.
- Make `permissions --request` request Input Monitoring by default. Retain `--input-monitoring` as a deprecated no-op for script compatibility.
- Input Monitoring remains optional: `record start` logs one warning and continues with workspace, Accessibility, visual, display, and audio signals when unavailable.
- Update `doctor`, daemon argument tests, README, and `CLAUDE.md` to describe event-driven capture, the five-second motion limit, graceful permission degradation, and removal of the ten-second safety snapshot.

## Test Plan

- Extend recorder tests with fake event sources to verify startup-only idle behavior, 350ms coalescing, per-display five-second throttling, bounded pending events, targeted versus all-display capture, retry/recovery, cancellation, and combined capture reasons.
- Verify clicks/scrolls without visual changes create no files, near-duplicates are removed before OCR/storage, semantic changes survive visual deduplication, and Accessibility/Vision failures still preserve written media.
- Test audio RMS hysteresis and ensure only activity edges create screen triggers.
- Extend native smoke coverage for monitor startup, display enumeration, optional event-tap availability, teardown, and hotplug rebuilding.
- Acceptance checks:
  - Thirty idle seconds produce only the startup baseline.
  - App switches and focused-window changes appear after settling.
  - Scrolling and clicks capture only when the display changes.
  - Changing video creates at most one retained frame per display every five seconds.
  - Recording continues with a clear warning when Input Monitoring is denied.
  - `task test`, `task vet`, and the permission-gated `task test:native` pass.

## Assumptions

- This change applies to screenshots and Accessibility/OCR work; WAV chunking and transcription remain continuous.
- Detection is generic and contains no Zoom, Teams, Meet, browser, or media-player-specific rules.
- No legacy interval-capture mode or unchanged heartbeat is retained.
- The existing baseline test suite is currently passing.
