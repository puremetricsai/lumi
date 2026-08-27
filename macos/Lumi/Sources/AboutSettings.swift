import AppKit
import ServiceManagement
import SwiftUI

/// AboutSettings is the identity tab: what this build is, whether it starts
/// with the Mac, and where the project lives.
///
/// It makes no network call. The three link rows and "Check now" hand a URL to
/// the user's browser, which is the user acting, not the app phoning home —
/// that distinction is what keeps Lumi's local-first promise literally true.
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
                // The mockup showed an "Automatic updates" toggle. The app has
                // no updater and is not getting one: re-running install.sh is
                // the update mechanism, and a second one would race it while
                // replacing the bundle this app is running from. The row keeps
                // its shape, but it promises only what it does — it opens the
                // releases page.
                LabeledContent("Check for new releases") {
                    Button("Check now") { open(Self.releases) }
                        .accessibilityLabel("Open the Lumi releases page on GitHub")
                }
                SettingsCaption("Lumi does not update itself. Check now opens the GitHub releases page, which lists what each release changed. Re-run the install command from the README to upgrade.")
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
