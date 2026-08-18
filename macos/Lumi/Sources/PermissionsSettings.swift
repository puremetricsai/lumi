import AppKit
import SwiftUI

/// PermissionsSettings is the read-only mirror of `lumi permissions`.
///
/// It shows status and nothing else. Requesting a permission belongs to the
/// Lumi window, which explains why access is needed before it raises a system
/// dialog; a settings list that prompted the moment it was opened would ask for
/// screen and microphone access with no context at all. So nothing here runs
/// `permissions --request`, and every read goes through
/// `RecorderController.refreshPermissions()`.
struct PermissionsSettings: View {
    @Environment(RecorderController.self) private var recorder

    /// The window's own focus, not the app's. Lumi is an accessory app with no
    /// Dock icon, so `NSApplication.didBecomeActiveNotification` fires on any
    /// menu-bar click, including while Settings is closed, and would need an
    /// observer installed and torn down by hand. This value is scoped to the
    /// view instead: it reads `.inactive` whenever Lumi is not frontmost, so
    /// coming back from System Settings is what triggers the re-read, and
    /// SwiftUI removes the observation with the view.
    @Environment(\.controlActiveState) private var activeState

    /// The general Privacy & Security pane. Per-service deep links live on
    /// `PermissionService.settingsURL`; this is the one destination that has no
    /// service to name.
    private static let privacyPaneURL = URL(
        string: "x-apple.systempreferences:com.apple.preference.security?Privacy")

    var body: some View {
        Form {
            // The rows come from `Permissions.rows` unchanged, because that is
            // the order `lumi permissions` prints. The design mockup lists
            // Microphone second; the CLI order wins, and this tab exists to
            // mirror the CLI. Do not re-sort it back to the mockup.
            Section("System access") {
                ForEach(recorder.permissions.rows) { row in
                    serviceRow(row)
                }
            }

            Section {
                Button("Open System Settings › Privacy & Security") {
                    guard let url = Self.privacyPaneURL else { return }
                    NSWorkspace.shared.open(url)
                }

                // The measured fact from docs/research/2026-08-17-tcc-spike.md,
                // Result 4: replacing the bundled `lumi` binary alone moves the
                // app's cdhash, and an ad-hoc designated requirement *is* that
                // cdhash. Without this line a developer reads a reset grant as
                // a bug in Lumi.
                SettingsCaption(
                    "This build is signed ad-hoc, so every rebuild is a new application to macOS. "
                    + "Permissions granted to the previous build stop applying and have to be granted again.")
            }
        }
        .formStyle(.grouped)
        .frame(minHeight: 420)
        .task { await recorder.refreshPermissions() }
        // A grant made in System Settings lands while this window is in the
        // background, so the list is re-read when the window comes back. No
        // timer: RecorderController already polls while capture is blocked on a
        // permission (see `updatePermissionPolling`), and a second timer here
        // would double the subprocess load and add nothing.
        .onChange(of: activeState) { _, state in
            guard state != .inactive else { return }
            Task { await recorder.refreshPermissions() }
        }
    }

    private func serviceRow(_ row: PermissionRow) -> some View {
        HStack(spacing: 10) {
            HStack(spacing: 12) {
                VStack(alignment: .leading, spacing: 2) {
                    Text(row.service.title)
                    SettingsCaption(row.service.subtitle)
                }
                Spacer(minLength: 12)
                StatePill(text: pillText(row), state: pillState(row))
            }
            // The title, subtitle and status read as one item. The chevron sits
            // outside that group so VoiceOver can still reach the button.
            .accessibilityElement(children: .combine)

            Button {
                guard let url = row.service.settingsURL else { return }
                NSWorkspace.shared.open(url)
            } label: {
                Image(systemName: "chevron.right")
                    .font(.system(size: 11, weight: .semibold))
                    .foregroundStyle(.secondary)
            }
            .buttonStyle(.borderless)
            .accessibilityLabel("Open \(row.service.title) in System Settings")
            .help("Open \(row.service.title) in System Settings")
        }
    }

    /// PermissionState.label is written for a service capture needs, so a
    /// denied one reads "Required" in red. Input Monitoring is never required —
    /// `Permissions.missingFor` never asks for it — and the spike's Result 6
    /// shows it lands on `denied` after every rebuild. A red "Required" there
    /// sends a developer to fix something that is not broken, so a denied
    /// optional service reads as plainly off instead.
    ///
    /// This softens one cell only. "Not requested" is already neutral and
    /// already true — nobody has asked yet is not the same as turned off — so
    /// it passes through unchanged, as does every granted state.
    private func pillText(_ row: PermissionRow) -> String {
        isQuietOptional(row) ? "Off" : row.state.label
    }

    private func pillState(_ row: PermissionRow) -> PermissionState {
        isQuietOptional(row) ? .notDetermined : row.state
    }

    private func isQuietOptional(_ row: PermissionRow) -> Bool {
        switch row.state {
        case .denied, .deniedOrNotDetermined, .restricted:
            return row.service.isOptional
        case .granted, .notDetermined:
            return false
        }
    }
}
