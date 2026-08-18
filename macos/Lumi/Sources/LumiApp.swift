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
        }
    }

    static let windowID = "lumi-main"
}

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    let recorder = RecorderController()
    private var statusItem: NSStatusItem?

    func applicationDidFinishLaunching(_ notification: Notification) {
        // Menu bar only. LSUIElement in Info.plist keeps it out of the Dock;
        // this makes the same statement to AppKit for a launch that bypassed
        // LaunchServices.
        NSApp.setActivationPolicy(.accessory)
        installStatusItem()

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
        item.button?.appearsDisabled = recorder.state != .recording
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

        let status = NSMenuItem(title: statusLine, action: nil, keyEquivalent: "")
        status.isEnabled = false
        status.image = Self.dotImage(for: recorder.state)
        menu.addItem(status)
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

    @objc private func quitApp() { Task { await quit() } }

    @objc private func openSettingsWindow() {
        NSApp.activate(ignoringOtherApps: true)
        // The selector differs by OS generation; both are tried so a rename
        // costs nothing at runtime.
        if NSApp.responds(to: Selector(("showSettingsWindow:"))) {
            NSApp.perform(Selector(("showSettingsWindow:")), with: nil)
        } else {
            NSApp.perform(Selector(("showPreferencesWindow:")), with: nil)
        }
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
