import SwiftUI

/// SettingsWindow is the standard Settings scene.
///
/// Every capture value here is stored in the app's own UserDefaults and
/// translated into recorder flags — Lumi has no configuration file and this
/// does not add one. The other tabs store nothing at all: they read the CLI and
/// show what it says.
///
/// The tab order is the brief's, and it runs from the setting people change
/// most often to the one they read once. Danger sits between MCP and
/// Permissions rather than last, so an irreversible action is never the tab a
/// stray click lands on.
struct SettingsWindow: View {
    @State private var selection = SettingsSelection.shared

    var body: some View {
        @Bindable var selection = selection
        TabView(selection: $selection.tab) {
            RecordingSettings()
                .tabItem { Label("Recording", systemImage: "record.circle") }
                .tag(SettingsSelection.Tab.recording)
            StorageSettings()
                .tabItem { Label("Storage", systemImage: "internaldrive") }
                .tag(SettingsSelection.Tab.storage)
            MCPSettings()
                .tabItem { Label("MCP", systemImage: "point.3.connected.trianglepath.dotted") }
                .tag(SettingsSelection.Tab.mcp)
            DangerSettings()
                .tabItem { Label("Danger", systemImage: "exclamationmark.triangle") }
                .tag(SettingsSelection.Tab.danger)
            PermissionsSettings()
                .tabItem { Label("Permissions", systemImage: "lock.shield") }
                .tag(SettingsSelection.Tab.permissions)
            AboutSettings()
                .tabItem { Label("About", systemImage: "info.circle") }
                .tag(SettingsSelection.Tab.about)
        }
        .frame(width: Theme.settingsWidth)
    }
}

/// SettingsSelection is which tab the Settings window is showing.
///
/// It exists because the window is opened from outside itself — the toolbar's
/// permissions warning wants the Permissions tab, not whichever tab was last
/// left open — and `OpenSettingsAction` takes no argument. Observable rather
/// than a value read once: the Settings scene is built once and reshown, so a
/// tab chosen at open time has to reach a `TabView` that already exists.
///
/// Stored nowhere. Which tab someone was last looking at is not a preference.
@Observable
@MainActor
final class SettingsSelection {
    enum Tab: String { case recording, storage, mcp, danger, permissions, about }

    static let shared = SettingsSelection()

    var tab: Tab = .recording
}

struct RecordingSettings: View {
    @Environment(RecorderController.self) private var recorder
    @State private var preferences = Preferences.shared

    /// The choices offered for each duration. The defaults match
    /// `recordFlags.bind` exactly: 2s and 30s.
    private let intervals: [Double] = [1, 2, 5, 10, 30]
    private let chunks: [Double] = [15, 30, 60, 120]
    private let locales = ["en-US", "en-GB", "de-DE", "es-ES", "fr-FR", "it-IT", "ja-JP", "pt-BR", "zh-CN"]

    var body: some View {
        Form {
            Section("Screen") {
                Toggle("Capture screen", isOn: Binding(
                    get: { preferences.captureScreen },
                    set: { preferences.captureScreen = $0; restart() }))
                SettingsCaption("All connected displays, hot-plug aware")

                Picker("Capture interval", selection: Binding(
                    get: { preferences.intervalSeconds },
                    set: { preferences.intervalSeconds = $0; restart() })) {
                    ForEach(intervals, id: \.self) { seconds in
                        Text(Self.label(seconds)).tag(seconds)
                    }
                }
                SettingsCaption("How often a changed frame is saved")
            }

            Section("Audio") {
                Toggle("Record audio", isOn: Binding(
                    get: { preferences.captureAudio },
                    set: { preferences.captureAudio = $0; restart() }))
                SettingsCaption("System output + microphone")

                Picker("Audio chunk length", selection: Binding(
                    get: { preferences.audioChunkSeconds },
                    set: { preferences.audioChunkSeconds = $0; restart() })) {
                    ForEach(chunks, id: \.self) { seconds in
                        Text(Self.label(seconds)).tag(seconds)
                    }
                }
                SettingsCaption("Length of each transcribed WAV segment. The level meters are live and do not depend on it.")

                Picker("Speech locale", selection: Binding(
                    get: { preferences.speechLocale },
                    set: { preferences.speechLocale = $0; restart() })) {
                    ForEach(locales, id: \.self) { locale in
                        Text(Self.localeLabel(locale)).tag(locale)
                    }
                }
                SettingsCaption("On-device transcription language")
            }
        }
        .formStyle(.grouped)
        .frame(minHeight: 420)
    }

    /// A capture setting only reaches the recorder through its flags, so
    /// changing one replaces the child. The restart goes through the same
    /// graceful stop as everything else — never a kill.
    private func restart() {
        // isSupervisingRecorder, not `state == .recording`: a permission
        // revoked mid-capture moves the UI to .needsPermissions while the child
        // is still running and still writing. Gating on the UI state there
        // would save the setting and leave the live recorder using the old
        // flags, with nothing to say the two had diverged.
        guard recorder.isSupervisingRecorder else { return }
        Task { await recorder.restart() }
    }

    static func label(_ seconds: Double) -> String {
        if seconds < 60 {
            return "\(Int(seconds)) second\(seconds == 1 ? "" : "s")"
        }
        let minutes = Int(seconds / 60)
        return "\(minutes) minute\(minutes == 1 ? "" : "s")"
    }

    static func localeLabel(_ identifier: String) -> String {
        let name = Locale.current.localizedString(forIdentifier: identifier) ?? identifier
        return "\(name) · \(identifier)"
    }
}
