import AppKit
import SwiftUI

/// LumiWindow is the floating toolbar behind "Open Lumi".
///
/// One capsule, no window chrome, no navigation, and no prose. Recording is the
/// normal state — Lumi is always-on, so there is no start/stop ceremony, no
/// timer, and no session name — which is why the whole app fits on a strip:
/// what a user comes here for is "is it capturing, and is it hearing anything".
///
/// The three states differ only in what sits between the mark and the gear.
/// Nothing here scrolls, wraps, or resizes; the window is exactly this row.
struct LumiWindow: View {
    @Environment(RecorderController.self) private var recorder
    @Environment(\.openSettings) private var openSettings

    var body: some View {
        HStack(spacing: Theme.barSpacing) {
            mark
            middle
            if let error = recorder.lastError {
                Image(systemName: "exclamationmark.triangle.fill")
                    .font(.system(size: 13))
                    .foregroundStyle(Theme.attention)
                    .help(error)
                    .accessibilityLabel(error)
            }
            settingsButton
            hideButton
        }
        .padding(.horizontal, 10)
        .frame(height: Theme.barHeight)
        // The row takes its ideal width, and the window is sized from that.
        // Without this the window proposes a width the row must fit into, and
        // an `HStack` shrinks the only flexible thing it holds — so "REC" and
        // the display count measured zero and silently did not draw, while
        // every icon and dot beside them looked correct.
        .fixedSize(horizontal: true, vertical: false)
        .background {
            // Material for the blur, then a dark wash over it: the toolbar is
            // dark whatever is behind it, so a white glyph is legible over a
            // pale desktop as well as a dark one.
            Capsule().fill(.regularMaterial)
            Capsule().fill(Color.black.opacity(0.4))
        }
        // The capsule is the whole window, so it carries its own dark
        // appearance rather than following the desktop's: the wash above is
        // what the glyph colours are chosen against.
        .preferredColorScheme(.dark)
        .background(WindowChrome())
        // The menu bar item and `lumi://settings` open Settings too, and the
        // delegate that serves them cannot read this action itself.
        .onAppear { LumiApp.openSettings = openSettings }
    }

    // MARK: - Mark

    /// The mark is the window's drag handle and nothing else.
    ///
    /// Drawn exactly as the menu bar draws it: the same template image, at the
    /// same secondary weight as every other glyph on the bar. It is not a
    /// button, so it takes no hover treatment — but it is not a logo either,
    /// and a solid white disc among translucent glyphs read as one.
    ///
    /// `isMovableByWindowBackground` is off, so this gesture is the only thing
    /// that moves the window — dragging a button must press it, not slide the
    /// toolbar out from under the pointer.
    private var mark: some View {
        Group {
            if let glyph = MenuBarGlyph.template {
                Image(nsImage: glyph)
                    .resizable()
                    .renderingMode(.template)
            }
        }
        .foregroundStyle(.secondary)
        // A touch larger than the 17pt glyphs beside it: the mark is a filled
        // disc where they are line art, and at the same size it reads smaller
        // than they do.
        .frame(width: 20, height: 20)
        .frame(width: Theme.barItemHeight, height: Theme.barItemHeight)
        .contentShape(Rectangle())
        .gesture(WindowDragGesture())
        .accessibilityLabel("Lumi. Drag to move the window")
    }

    // MARK: - States

    /// One row per state, so the recording case's own spacing is stated where
    /// its contents are rather than inherited from the bar it sits in.
    @ViewBuilder
    private var middle: some View {
        switch recorder.state {
        case .recording:
            HStack(spacing: Theme.barSpacing) {
                recordingPill
                if Preferences.shared.captureScreen { displayPill }
                if Preferences.shared.captureAudio {
                    audioPill(symbol: "mic", title: "Microphone", source: "microphone",
                              silenceIsFailure: true)
                    audioPill(symbol: "speaker.wave.2", title: "System audio", source: "system")
                }
                stopButton
            }
        case .idle:
            recordButton
        case .needsPermissions:
            permissionsButton
        }
    }

    private var recordingPill: some View {
        HStack(spacing: 6) {
            StatusDot(color: Theme.recording, pulsing: true, diameter: 7)
            Text("REC")
                .font(.system(size: 11, weight: .bold))
                .kerning(0.5)
                .foregroundStyle(Theme.recording)
        }
        .padding(.horizontal, 10)
        .frame(height: Theme.barItemHeight)
        .background(Capsule().fill(Theme.recording.opacity(0.16)))
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("Recording")
    }

    /// The live sources are behind the same preference gates the recorder itself
    /// is started with: a track that was never asked for has no pill.
    ///
    /// The count is the recorder's, reported per tick, and a missing report is
    /// drawn as missing for the same reason `audioPill` draws a missing level
    /// that way. `NSScreen.screens.count` is deliberately not the fallback: it
    /// answers how many displays are *connected*, which stopped being the same
    /// question the moment a display selection became possible, and a number
    /// from it beside a green dot would be a claim about capture nobody
    /// measured. The first tick is immediate, so this clears within seconds of
    /// capture starting.
    private var displayPill: some View {
        let tick = recorder.screenCapture
        let count = tick?.displayIds.count
        let detail = count == nil ? "No signal yet"
            : tick?.selectionFallback == true
                ? "recording every display — the selected ones are not connected"
                : count == 1 ? "1 display" : "\(count!) displays"
        return ToolbarPill {
            Image(systemName: "display").font(.system(size: 12))
            Text(count.map(String.init) ?? "—").font(.system(size: 12, weight: .medium))
            StatusDot(color: count == nil ? Theme.attention : Theme.live, diameter: 6)
        }
        .help("Screen capture — \(detail)")
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("Screen capture, \(detail)")
    }

    /// audioPill is one audio track.
    ///
    /// A missing level is drawn as a missing level. `RecorderController` prunes a
    /// measurement the recorder has stopped refreshing, so nil here means one of
    /// two things — capture has only just started, or the track has stopped
    /// reporting because the microphone is denied, absent, or gone — and neither
    /// is "sound was heard". Bars at their floor beside a green dot said the
    /// opposite of both, and a dead microphone was indistinguishable from a quiet
    /// room. Levels are live, so this clears within a second of capture starting.
    ///
    /// The recorder's own `silent` means opposite things on the two tracks, which
    /// is `silenceIsFailure`. Nothing playing is what a system track sounds like
    /// most of the time; a live microphone in a silent room still carries its own
    /// noise, so a microphone reporting silence is one nothing is reaching — a
    /// denied or stale Microphone grant delivers empty buffers rather than an
    /// error, and this pill is the only place that shows.
    private func audioPill(
        symbol: String, title: String, source: String, silenceIsFailure: Bool = false
    ) -> some View {
        let level = recorder.level(for: source)
        let digitallySilent = silenceIsFailure && level?.silent == true
        let healthy = level != nil && !digitallySilent
        let detail = level == nil ? "No signal yet"
            : digitallySilent ? "Silent — check Microphone access"
            : "peak \(Int(level!.peakDbfs)) decibels full scale"
        return ToolbarPill {
            Image(systemName: symbol).font(.system(size: 12))
            LevelMeter(level: level)
            StatusDot(color: healthy ? Theme.live : Theme.attention, diameter: 6)
        }
        // The sentence the row used to print. There is no width for it here, so
        // it is the tooltip and the accessibility label instead of being lost.
        .help("\(title) — \(detail)")
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("\(title), \(detail)")
    }

    // MARK: - Buttons

    private var recordButton: some View {
        Button {
            Task { await recorder.start() }
        } label: {
            Image(systemName: "circle.fill").font(.system(size: 15))
        }
        .buttonStyle(ToolbarButtonStyle(tint: .white, hoverTint: Theme.recording))
        .accessibilityLabel("Start recording")
        .help("Start recording")
    }

    private var stopButton: some View {
        Button {
            Task { await recorder.stop() }
        } label: {
            Image(systemName: "stop.fill").font(.system(size: 14))
        }
        .buttonStyle(ToolbarButtonStyle(tint: .white, hoverTint: Theme.recording))
        .disabled(recorder.isStopping)
        .opacity(recorder.isStopping ? 0.4 : 1)
        .accessibilityLabel(recorder.isStopping ? "Stopping" : "Stop recording")
        .help(recorder.isStopping ? "Stopping…" : "Stop recording")
    }

    /// The permissions state is one button.
    ///
    /// It runs the request flows first — Microphone and Speech Recognition
    /// answer with a dialog, and this is their only caller — and then opens
    /// Settings on the Permissions tab, which carries a System Settings route
    /// per service. Both, because they fix disjoint halves: Screen Recording and
    /// Accessibility never ask twice once macOS holds a decision for this build,
    /// so no amount of requesting reaches them.
    private var permissionsButton: some View {
        Button {
            Task {
                await recorder.requestPermissions()
                guard recorder.state == .needsPermissions else { return }
                SettingsSelection.shared.tab = .permissions
                openSettings()
            }
        } label: {
            Image(systemName: "exclamationmark.triangle.fill").font(.system(size: 13))
        }
        .buttonStyle(ToolbarButtonStyle(tint: Theme.attention, hoverTint: Theme.attention))
        .accessibilityLabel("Permissions needed. Grant system access")
        .help("Lumi needs system access before it can capture")
    }

    private var settingsButton: some View {
        Button {
            openSettings()
        } label: {
            Image(systemName: "gearshape").font(.system(size: 14))
        }
        .buttonStyle(ToolbarButtonStyle())
        .accessibilityLabel("Open Settings")
        .help("Settings")
    }

    /// The x puts the toolbar away; it does not quit Lumi.
    ///
    /// An x in the corner of a window is read as "quit" by anyone who has met a
    /// menu bar app that hides in one, which is why the tooltip says what
    /// happens to capture. Lumi is quit from the menu bar and nowhere else, so
    /// this takes the plain tint rather than the red reserved for controls that
    /// end something.
    private var hideButton: some View {
        Button {
            LumiApp.hide?()
        } label: {
            Image(systemName: "xmark").font(.system(size: 13, weight: .medium))
        }
        .buttonStyle(ToolbarButtonStyle())
        .accessibilityLabel("Hide Lumi")
        .help("Hide the toolbar. Recording continues.")
    }
}

/// WindowChrome strips the window down to the capsule the toolbar draws.
///
/// `.windowStyle(.plain)` removes the title bar and the traffic lights but
/// leaves an opaque background behind the capsule's rounded corners, and there
/// is no SwiftUI API for a window's level or for turning background-dragging
/// off. All four settings are AppKit-only, so this reaches the `NSWindow` once,
/// when the view is installed in it.
private struct WindowChrome: NSViewRepresentable {
    func makeNSView(context: Context) -> NSView { Configurator() }

    func updateNSView(_ nsView: NSView, context: Context) {}

    private final class Configurator: NSView {
        private var escapeMonitor: Any?

        deinit {
            if let escapeMonitor { NSEvent.removeMonitor(escapeMonitor) }
        }

        override func viewDidMoveToWindow() {
            super.viewDidMoveToWindow()
            guard let window else { return }
            installEscapeMonitor(for: window)
            window.hasShadow = true
            // `.plain` leaves a borderless window, and a borderless window
            // cannot become key — so no key press reaches it at all and the
            // toolbar has no keyboard route out. `.titled` restores that
            // without restoring any chrome: `.fullSizeContentView` keeps the
            // content the whole window, and the title bar it would otherwise
            // draw is made transparent and emptied of its buttons.
            window.styleMask.insert([.titled, .fullSizeContentView])
            window.titlebarAppearsTransparent = true
            window.titleVisibility = .hidden
            for button in [NSWindow.ButtonType.closeButton, .miniaturizeButton, .zoomButton] {
                window.standardWindowButton(button)?.isHidden = true
            }
            window.isOpaque = false
            window.backgroundColor = .clear
            // Above every other app: the toolbar says whether capture is live,
            // which is worth nothing behind the window being captured.
            window.level = .floating
            // The mark is the drag handle. Dragging the background would make
            // every button a place the window slides from.
            window.isMovableByWindowBackground = false
            // "Open Lumi" must open it where the user is. A window left on the
            // Space it was created in reports `isVisible` while being on
            // nobody's screen, and with no traffic lights and no Dock icon
            // there is nothing to find it with.
            window.collectionBehavior.insert(.moveToActiveSpace)
        }

        /// Esc is handled here rather than with `onExitCommand`.
        ///
        /// SwiftUI delivers a cancel action through whatever holds focus, and
        /// this window holds none: no text field, no list, nothing focusable
        /// but buttons nobody has tabbed to. A local monitor sees the key down
        /// on its way into the app instead, which needs no focus at all — only
        /// a key window, which is the one condition no mechanism can escape.
        /// It returns nil so AppKit does not beep at an unhandled Esc.
        private func installEscapeMonitor(for window: NSWindow) {
            guard escapeMonitor == nil else { return }
            escapeMonitor = NSEvent.addLocalMonitorForEvents(matching: .keyDown) { event in
                guard event.keyCode == 53, event.window === window else { return event }
                Task { @MainActor in LumiApp.hide?() }
                return nil
            }
        }
    }
}
