import AppKit
import ServiceManagement
import SwiftUI

/// AboutSettings is the identity tab: what this build is, whether it starts
/// with the Mac, and where the project lives.
///
/// It is also the one place in Lumi that touches the network, and the only one
/// that ever will: `lumi update` sends a bare GET to github.com carrying nothing
/// but the request, to learn the newest release tag. The three link rows hand a
/// URL to the user's browser instead, which is the user acting rather than the
/// app phoning home. The update check is the app acting, so it is named plainly
/// below and can be switched off.
struct AboutSettings: View {
    /// The version reported by the bundled binary, or nil while it is being
    /// read. `internal/cli` owns this string and release sets it with
    /// `-ldflags`, so the binary is asked rather than the bundle: a stale
    /// CFBundleShortVersionString would name a build that is not the one
    /// actually running captures.
    @State private var version: String?
    @State private var versionError: String?

    /// The full-colour mark. Loaded off the main thread, and absent until it
    /// arrives, so the first draw never waits on the disk.
    @State private var logo: NSImage?

    /// What launchd holds for this app, or nil until the first read. The system
    /// is the only source of truth here; nothing about login items is mirrored
    /// into `Preferences`, because a stored copy can disagree with launchd and
    /// there is no way for the user to tell which one is lying.
    @State private var loginStatus: SMAppService.Status?
    @State private var loginError: String?

    /// Owned by `AppDelegate` and injected, like the recorder: the menu bar item
    /// and this tab must read the same answer, and a second checker would make
    /// two requests to disagree with each other.
    @Environment(UpdateChecker.self) private var updates
    @State private var preferences = Preferences.shared

    /// The delegate owns the confirmation and the shutdown that follows it, so
    /// the button here routes to it rather than restating either.
    private var delegate: AppDelegate? { NSApp.delegate as? AppDelegate }

    private static let repository = URL(string: "https://github.com/puremetricsai/lumi")!
    private static let documentation = URL(string: "https://github.com/puremetricsai/lumi#readme")!
    private static let issues = URL(string: "https://github.com/puremetricsai/lumi/issues/new")!
    private static let releases = URL(string: "https://github.com/puremetricsai/lumi/releases")!

    var body: some View {
        Form {
            Section {
                header
            }

            Section {
                Toggle("Launch at login", isOn: Binding(
                    get: { loginStatus == .enabled },
                    set: { setLaunchAtLogin($0) }))
                    // Disabled only until the first status read returns. A
                    // toggle drawn before launchd has answered would show "off"
                    // for an app that is in fact registered.
                    .disabled(loginStatus == nil)
                SettingsCaption("Start Lumi in the menu bar on sign-in")

                if let note = loginStatusNote {
                    SettingsCaption(note)
                }
                if let loginError {
                    Text(loginError)
                        .font(.caption)
                        .foregroundStyle(Theme.recording)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }

            Section("Updates") {
                // This used to be a browser link, because the app had no
                // updater. It has one now, and it is not a second install
                // channel: `lumi update --apply` runs install.sh — the same
                // script, the same URL, the same `latest` pointer the check
                // itself resolved. Swift holds none of that. It asks the binary
                // what is available and tells it to go.
                LabeledContent("Status") {
                    updateBadge
                }

                LabeledContent("Check for new releases") {
                    HStack(spacing: 8) {
                        if updates.status?.updateAvailable == true {
                            Button("Install Update…") {
                                Task { await delegate?.confirmAndInstallUpdate() }
                            }
                            .accessibilityLabel("Stop recording, install the update, and reopen Lumi")
                        }
                        Button("Check Now") { Task { await updates.check(userInitiated: true) } }
                            .disabled(updates.isChecking)
                            .accessibilityLabel("Ask GitHub whether a newer Lumi has been released")
                    }
                }

                Toggle("Check for updates automatically", isOn: Binding(
                    get: { preferences.checkForUpdates },
                    set: { preferences.checkForUpdates = $0 }))
                SettingsCaption("Once a day, Lumi asks github.com for the newest release tag. The request carries nothing — no account, no machine identifier, no usage. Turn this off and Lumi never contacts the network on its own.")

                link("Release notes", url: Self.releases,
                     accessibility: "Open the Lumi releases page on GitHub")

                if let reason = updates.status?.reason, updates.status?.latest == nil {
                    SettingsCaption(reason)
                }
                if let lastError = updates.lastError {
                    Text(lastError)
                        .font(.caption)
                        .foregroundStyle(Theme.attention)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }

            Section("Links") {
                link("GitHub", url: Self.repository,
                     accessibility: "Open the Lumi repository on GitHub")
                link("Documentation", url: Self.documentation,
                     accessibility: "Open the Lumi documentation on GitHub")
                link("Report an issue", url: Self.issues,
                     accessibility: "Open a new issue on GitHub")
            }
        }
        .formStyle(.grouped)
        .frame(minHeight: 420)
        .task {
            await loadLogo()
            await loadVersion()
            await refreshLoginStatus()
        }
    }

    /// The status line. "Unknown" until the first check returns, rather than
    /// "up to date" — claiming a version is current before anything was asked is
    /// the one answer here that could be wrong without anybody noticing.
    private var updateBadge: some View {
        if updates.isChecking {
            return Badge(text: "Checking…", tone: .neutral)
        }
        guard let status = updates.status else {
            return Badge(text: updates.lastError == nil ? "Not checked yet" : "Check failed",
                         tone: updates.lastError == nil ? .neutral : .warn)
        }
        if status.updateAvailable, let latest = status.latest {
            return Badge(text: "\(latest) available", tone: .warn)
        }
        // `latest` is set exactly when a comparison happened, so its absence --
        // not the wording of `reason` -- is what says nothing was compared. A
        // development build lands here, and calling it "up to date" would claim
        // a currency nobody established. Reading the reason string to decide
        // this would put Go's vocabulary in Swift, which is the drift
        // `macos/CLAUDE.md` forbids.
        guard status.latest != nil else {
            return Badge(text: "Not applicable", tone: .neutral)
        }
        return Badge(text: "Up to date", tone: .ok)
    }

    // MARK: - Header

    private var header: some View {
        VStack(spacing: 6) {
            mark
                .frame(width: 64, height: 64)
                // The word "lumi" sits directly below and carries the name, so
                // announcing the mark as well would repeat it.
                .accessibilityHidden(true)

            Text("lumi")
                .font(.system(size: 20, weight: .semibold))

            Text(versionLine)
                .font(.caption)
                .foregroundStyle(.secondary)

            if let versionError {
                Text(versionError)
                    .font(.caption)
                    .foregroundStyle(Theme.attention)
                    .multilineTextAlignment(.center)
                    .fixedSize(horizontal: false, vertical: true)
            }

            Text("Local-first work memory · MIT License")
                .font(.caption)
                .foregroundStyle(.secondary)
        }
        .frame(maxWidth: .infinity)
        .padding(.vertical, 10)
    }

    /// The logo, or a system symbol when the resource is missing. Same shape as
    /// `AppDelegate.menuBarGlyph()`: a missing file costs the icon, never the
    /// tab. Unlike the menu bar, this one is drawn in full colour — the mark is
    /// amber here, not a template mask.
    @ViewBuilder
    private var mark: some View {
        if let logo {
            Image(nsImage: logo)
                .resizable()
                .interpolation(.high)
                .aspectRatio(contentMode: .fit)
        } else {
            Image(systemName: "circle.fill")
                .resizable()
                .aspectRatio(contentMode: .fit)
                .foregroundStyle(Theme.accent)
        }
    }

    private var versionLine: String {
        // "Apple Silicon · macOS 26+" is the platform gate itself, not a build
        // fact: `platform.Validate` refuses anything else, and the app is
        // compiled for arm64-apple-macosx26.0.
        "Version \(version ?? "…") · Apple Silicon · macOS 26+"
    }

    // MARK: - Rows

    private func link(_ title: String, url: URL, accessibility: String) -> some View {
        Button {
            open(url)
        } label: {
            HStack {
                Text(title)
                Spacer()
                Image(systemName: "arrow.up.forward")
                    .font(.system(size: 11, weight: .semibold))
                    .foregroundStyle(.secondary)
            }
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .pointerStyle(.link)
        .accessibilityLabel(accessibility)
    }

    /// The two launchd states that are neither on nor off, each explained where
    /// the toggle cannot show them.
    private var loginStatusNote: String? {
        switch loginStatus {
        case .some(.requiresApproval):
            return "macOS is holding this. Approve Lumi in System Settings → General → Login Items."
        case .some(.notFound):
            return "The system has no login item for this copy of Lumi. Move Lumi.app to your Applications folder and try again."
        default:
            return nil
        }
    }

    // MARK: - Work

    private func open(_ url: URL) {
        NSWorkspace.shared.open(url)
    }

    private func loadLogo() async {
        guard let url = Bundle.main.url(forResource: "AppIcon", withExtension: "png") else { return }
        let data = await Task.detached(priority: .userInitiated) {
            try? Data(contentsOf: url)
        }.value
        guard let data else { return }
        logo = NSImage(data: data)
    }

    private func loadVersion() async {
        do {
            // No --data-dir: asking the binary its own version must not depend
            // on a store existing or being readable.
            let result = try await LumiCLI.run(["version"], includeDataDirectory: false)
            let text = result.stdout.trimmingCharacters(in: .whitespacesAndNewlines)
            guard result.succeeded, !text.isEmpty else {
                throw LumiCLI.Failure.command(
                    arguments: ["version"], status: result.status, stderr: result.stderr)
            }
            version = text
            versionError = nil
        } catch {
            version = "unavailable"
            versionError = error.localizedDescription
        }
    }

    /// Reading `.status` talks to launchd, so it is kept off the main thread
    /// like every other out-of-process question the app asks.
    private func refreshLoginStatus() async {
        loginStatus = await Task.detached(priority: .userInitiated) {
            SMAppService.mainApp.status
        }.value
    }

    /// Both `register()` and `unregister()` throw, and with an ad-hoc signature
    /// a refusal is a real possibility rather than a theoretical one. So the
    /// error is shown, and the status is re-read either way: the toggle must
    /// report what launchd holds, never what the click asked for.
    private func setLaunchAtLogin(_ enabled: Bool) {
        Task {
            loginError = nil
            do {
                try await Task.detached(priority: .userInitiated) {
                    let service = SMAppService.mainApp
                    if enabled {
                        try service.register()
                    } else {
                        try service.unregister()
                    }
                }.value
            } catch {
                loginError = enabled
                    ? "Could not enable launch at login: \(error.localizedDescription)"
                    : "Could not disable launch at login: \(error.localizedDescription)"
            }
            await refreshLoginStatus()
        }
    }
}
