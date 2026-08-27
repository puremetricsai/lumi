import AppKit
import SwiftUI

@main
struct LumiApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var delegate

    var body: some Scene {
        // The window opens on demand and closing it never stops capture:
        // recording is the normal state, and quitting is the only thing that
        // ends it.
        Window("Lumi", id: LumiApp.windowID) {
            LumiWindow()
                .environment(delegate.recorder)
        }
        .windowStyle(.hiddenTitleBar)
        .windowResizability(.contentSize)
        .defaultPosition(.center)
        .commandsRemoved()

        Settings {
            SettingsWindow()
                .environment(delegate.recorder)
                .environment(delegate.updates)
        }
    }

    static let windowID = "lumi-main"
}

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    let recorder = RecorderController()
    let updates = UpdateChecker()
    private var statusItem: NSStatusItem?

    func applicationDidFinishLaunching(_ notification: Notification) {
        // Menu bar only. LSUIElement in Info.plist keeps it out of the Dock;
        // this makes the same statement to AppKit for a launch that bypassed
        // LaunchServices.
        NSApp.setActivationPolicy(.accessory)
        recorder.statusDidChange = { [weak self] in self?.updateStatusItem() }
        // An available update is a menu bar item, so the same hook drives it.
        updates.statusDidChange = { [weak self] in self?.updateStatusItem() }
        installStatusItem()

        updates.startAutomaticChecks()

        Task {
            await recorder.refreshPermissions()
            if recorder.state == .needsPermissions {
                // Explained before requested: the window opens on the
                // permissions state and recording stays paused. Nothing here
                // triggers a system prompt.
                showWindow()
            } else {
                await recorder.start()
            }
            updateStatusItem()
        }

        // Re-check on activation so a grant made in System Settings clears
        // without relaunching the app.
        NotificationCenter.default.addObserver(
            forName: NSApplication.didBecomeActiveNotification, object: nil, queue: .main
        ) { [weak self] _ in
            Task { @MainActor [weak self] in
                await self?.recorder.refreshPermissions()
                self?.updateStatusItem()
            }
        }
    }

    /// Closing the last window must not quit: capture continues with everything
    /// closed, which is the point of a menu bar app.
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }

    /// applicationShouldTerminate holds termination open while the recorder is
    /// stopped gracefully. Returning .terminateNow here would cut the SIGTERM
    /// wait short and lose in-flight media.
    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        guard recorder.isSupervisingRecorder else { return .terminateNow }
        Task {
            await recorder.stop()
            NSApp.reply(toApplicationShouldTerminate: true)
        }
        return .terminateLater
    }

    // MARK: - Status item

    private func installStatusItem() {
        let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        item.button?.image = Self.menuBarGlyph()
        item.button?.setAccessibilityLabel("Lumi")
        item.menu = buildMenu()
        statusItem = item
        updateStatusItem()
    }

    /// menuBarGlyph loads the "l" mark from the bundle, falling back to a
    /// system symbol so a missing resource costs the icon rather than the app.
    /// It ships as a template image, which is what makes it follow the menu
    /// bar's own appearance in light and dark.
    private static func menuBarGlyph() -> NSImage? {
        if let url = Bundle.main.url(forResource: "menubar-glyph", withExtension: "png"),
           let glyph = MenuBarGlyph.make(from: url) {
            return glyph
        }
        let fallback = NSImage(systemSymbolName: "circle.fill", accessibilityDescription: "Lumi")
        fallback?.isTemplate = true
        return fallback
    }

    func updateStatusItem() {
        guard let item = statusItem else { return }
        // Highlighted while recording, so the menu bar itself says whether
        // capture is live without opening anything.
        item.button?.appearsDisabled = recorder.state != .recording || recorder.isStopping
        item.menu = buildMenu()
    }

    /// buildMenu builds the plain menu: a status line, the two things worth
    /// opening, and Quit last behind a separator.
    ///
    /// Quit is the one destructive item here, so it is separated from the two
    /// that merely open a window — the separator is what stops it being hit by
    /// a click aimed at Open Settings. It still goes through
    /// `AppDelegate.quit()`, which asks first while capture is live and always
    /// takes the graceful SIGTERM-then-wait path; a menu item that terminated
    /// directly would discard media that was written but not yet indexed.
    private func buildMenu() -> NSMenu {
        let menu = NSMenu()
        // Explicit enablement: AppKit's auto-enabling ignores `isEnabled` for any
        // item whose target responds to its action, which would re-enable the
        // toggle in the states where it does nothing.
        menu.autoenablesItems = false

        let status = NSMenuItem(title: statusLine, action: nil, keyEquivalent: "")
        status.isEnabled = false
        status.image = Self.dotImage(for: recorder.isStopping ? .idle : recorder.state)
        menu.addItem(status)

        // Titled and dispatched on `isSupervisingRecorder`, never on `state`. A
        // permission revoked mid-capture moves the UI to `.needsPermissions`
        // while the child keeps running and writing, and reading `state` there
        // would offer "Start Recording" beside a live recorder.
        //
        // Two selectors rather than one that decides when it runs, because the
        // title and the action must not be able to disagree. Reassigning
        // `item.menu` cannot reach a menu that is already open, so a child that
        // exits while the dropdown is held leaves "Stop Recording" on screen —
        // and a single action re-reading the recorder at click time would start
        // one instead. Deciding here makes a stale click land in the guard at
        // the top of `start()`/`stop()` and do nothing, which is the only
        // honest outcome for a button whose label is out of date.
        let recording = recorder.isSupervisingRecorder
        let toggle = NSMenuItem(
            title: recording ? "Stop Recording" : "Start Recording",
            action: recording ? #selector(stopRecording) : #selector(startRecording),
            keyEquivalent: "")
        toggle.target = self
        toggle.isEnabled = !recorder.isStopping
            && !(recorder.state == .needsPermissions && !recorder.isSupervisingRecorder)
        menu.addItem(toggle)

        // Present only when there is an update, rather than greyed out when
        // there is not: an item that never does anything on most days is
        // clutter, and this menu is four lines long on purpose.
        if updates.status?.updateAvailable == true, let latest = updates.status?.latest {
            let update = NSMenuItem(
                title: "Update to \(latest)…", action: #selector(installUpdate), keyEquivalent: "")
            update.target = self
            menu.addItem(update)
        }

        menu.addItem(.separator())

        let open = NSMenuItem(title: "Open Lumi", action: #selector(openWindow), keyEquivalent: "")
        open.target = self
        menu.addItem(open)

        let settings = NSMenuItem(title: "Open Settings", action: #selector(openSettingsWindow), keyEquivalent: ",")
        settings.target = self
        menu.addItem(settings)

        menu.addItem(.separator())

        // ⌘Q, because that is what someone types to quit an app and this menu
        // is the only place the app can be quit from. It is routed to `quit`
        // rather than to NSApp.terminate: terminate would still reach the
        // graceful path through applicationShouldTerminate, but it would skip
        // the confirmation that stops an accidental ⌘Q ending a live capture.
        let quit = NSMenuItem(title: "Quit Lumi", action: #selector(quitApp), keyEquivalent: "q")
        quit.target = self
        menu.addItem(quit)
        return menu
    }

    private var statusLine: String {
        if recorder.isStopping { return "Stopping…" }
        switch recorder.state {
        case .recording: return "Live · recording"
        case .idle: return "Not recording"
        case .needsPermissions: return "Paused · needs permissions"
        }
    }

    private static func dotImage(for state: RecorderController.State) -> NSImage {
        let color: NSColor
        switch state {
        case .recording: color = .systemGreen
        case .idle: color = .systemGray
        case .needsPermissions: color = .systemOrange
        }
        let size = NSSize(width: 9, height: 9)
        let image = NSImage(size: size, flipped: false) { rect in
            color.setFill()
            NSBezierPath(ovalIn: rect).fill()
            return true
        }
        // Never a template: the colour is the information.
        image.isTemplate = false
        return image
    }

    // MARK: - Actions and deep links

    @objc private func openWindow() { showWindow() }

    /// startRecording and stopRecording run the same supervisor methods the Lumi
    /// window's buttons do, so the graceful SIGTERM-then-wait stop is shared
    /// rather than restated. Each rebuilds the status item afterwards because a
    /// `start()` that fails to launch changes only `lastError`, firing no status
    /// hook of its own.
    @objc private func startRecording() {
        Task {
            await recorder.start()
            updateStatusItem()
        }
    }

    @objc private func stopRecording() {
        Task {
            await recorder.stop()
            updateStatusItem()
        }
    }

    @objc private func quitApp() { Task { await quit() } }

    @objc private func installUpdate() { Task { await confirmAndInstallUpdate() } }

    @objc private func openSettingsWindow() {
        NSApp.activate(ignoringOtherApps: true)
        NSApp.perform(Selector(("showSettingsWindow:")), with: nil)
    }

    func showWindow() {
        NSApp.activate(ignoringOtherApps: true)
        // Raise the window that already exists rather than asking for another:
        // `lumi app` promises never to start a second copy, and the same
        // promise holds for the window.
        if let existing = NSApp.windows.first(where: { $0.identifier?.rawValue.contains(LumiApp.windowID) == true }) {
            existing.makeKeyAndOrderFront(nil)
            return
        }
        NSApp.windows.first?.makeKeyAndOrderFront(nil)
    }

    /// application(_:open:) handles the `lumi://` URLs that `lumi app` sends.
    ///
    /// A URL is what reaches an app that is already running — the normal state
    /// for a menu bar app — which `open -a … --args` cannot do.
    func application(_ application: NSApplication, open urls: [URL]) {
        for url in urls where url.scheme == "lumi" {
            switch url.host() ?? url.path.trimmingCharacters(in: CharacterSet(charactersIn: "/")) {
            case "settings": openSettingsWindow()
            case "quit": Task { await quit() }
            default: showWindow()
            }
        }
    }

    /// confirmAndInstallUpdate asks once, then hands the upgrade to the binary
    /// and quits.
    ///
    /// One alert, not two. It deliberately does not call `quit()`, which would
    /// put its own "Stop recording and quit Lumi?" on top of the question the
    /// user has already answered — it takes the same graceful stop directly.
    /// `applicationShouldTerminate` then sees no child held and returns
    /// `.terminateNow`, so nothing asks a third time.
    ///
    /// The app quits *itself* rather than letting install.sh do it by Apple
    /// event: a self-directed event would cost a needless Automation prompt.
    /// install.sh needs no change — by the time it looks, the recorder has
    /// already stopped and the app is on its way out, so its quit block finds
    /// nothing or waits out the last of the shutdown.
    func confirmAndInstallUpdate() async {
        guard let status = updates.status, status.updateAvailable else { return }
        let version = status.latest ?? "the latest release"

        let alert = NSAlert()
        alert.messageText = "Install Lumi \(version)?"
        alert.informativeText =
            "Lumi will stop recording, install \(version), and reopen. Anything already "
            + "captured is indexed first. The installer writes what it did to update.log in "
            + "your Lumi data folder."
        alert.addButton(withTitle: "Install and Restart")
        alert.addButton(withTitle: "Cancel")
        NSApp.activate(ignoringOtherApps: true)
        guard alert.runModal() == .alertFirstButtonReturn else { return }

        // Whether capture was actually running, read before it is stopped: the
        // update can be taken with recording deliberately off, and `stop()`
        // no-ops there, so an unconditional restart below would switch capture
        // *on* for somebody who had switched it off.
        let wasRecording = recorder.isSupervisingRecorder

        // The recorder stops *before* the installer is started, not after.
        // install.sh quits a running Lumi by Apple event, and
        // `applicationShouldTerminate` answers that by stopping and then
        // replying `true` whatever the stop returned — so an installer racing a
        // slow shutdown can carry the app out from over a child that is still
        // writing. Nothing can race a shutdown that has already finished.
        await recorder.stop()
        // A stop that timed out leaves the child alive and still writing, and
        // media that was captured but not yet indexed is exactly what the
        // graceful path exists to save. Nothing has been downloaded or replaced
        // at this point, so stopping here costs the update and nothing else.
        if recorder.stopFailed {
            let failure = NSAlert()
            failure.messageText = "Lumi is still finishing its recording."
            failure.informativeText =
                "Nothing has been changed. Try the update again once Lumi has finished "
                + "indexing what it captured."
            failure.addButton(withTitle: "OK")
            failure.runModal()
            return
        }

        do {
            try await updates.apply()
        } catch {
            // The binary refuses before it downloads or replaces anything, so
            // the only thing spent is the recording that was just stopped for
            // an update that is not going to happen. Start it again rather than
            // leaving capture silently off.
            let failure = NSAlert()
            failure.messageText = "Lumi could not install this update."
            failure.informativeText = error.localizedDescription
            failure.addButton(withTitle: "OK")
            failure.runModal()
            if wasRecording {
                await recorder.start()
                updateStatusItem()
            }
            return
        }

        NSApp.terminate(nil)
    }

    /// quit stops the recorder before terminating, and asks first when that
    /// would interrupt a live capture.
    func quit() async {
        if recorder.isSupervisingRecorder {
            let alert = NSAlert()
            alert.messageText = "Stop recording and quit Lumi?"
            alert.informativeText =
                "Lumi will finish indexing anything it has already captured before it exits. "
                + "Nothing is recorded while Lumi is not running."
            alert.addButton(withTitle: "Quit")
            alert.addButton(withTitle: "Cancel")
            NSApp.activate(ignoringOtherApps: true)
            guard alert.runModal() == .alertFirstButtonReturn else { return }
        }
        await recorder.stop()
        NSApp.terminate(nil)
    }
}
