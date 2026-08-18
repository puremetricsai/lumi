import SwiftUI

/// SettingsWindow is the standard Settings scene.
///
/// Only the Recording tab exists so far; the remaining five arrive with the
/// Settings deliverable. Every value here is stored in the app's own
/// UserDefaults and translated into recorder flags — Lumi has no configuration
/// file and this does not add one.
struct SettingsWindow: View {
    var body: some View {
        TabView {
            RecordingSettings()
                .tabItem { Label("Recording", systemImage: "record.circle") }
        }
        .frame(width: Theme.settingsWidth)
    }
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
                    .help("All connected displays, hot-plug aware")
                Text("All connected displays, hot-plug aware")
                    .font(.caption)
                    .foregroundStyle(.secondary)

                Picker("Capture interval", selection: Binding(
                    get: { preferences.intervalSeconds },
                    set: { preferences.intervalSeconds = $0; restart() })) {
                    ForEach(intervals, id: \.self) { seconds in
                        Text(Self.label(seconds)).tag(seconds)
                    }
                }
                Text("How often a changed frame is saved")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }

            Section("Audio") {
                Toggle("Record audio", isOn: Binding(
                    get: { preferences.captureAudio },
                    set: { preferences.captureAudio = $0; restart() }))
                Text("System output + microphone")
                    .font(.caption)
                    .foregroundStyle(.secondary)

                Picker("Audio chunk length", selection: Binding(
                    get: { preferences.audioChunkSeconds },
                    set: { preferences.audioChunkSeconds = $0; restart() })) {
                    ForEach(chunks, id: \.self) { seconds in
                        Text(Self.label(seconds)).tag(seconds)
                    }
                }
                Text("Length of each transcribed WAV segment. Level meters refresh once per segment.")
                    .font(.caption)
                    .foregroundStyle(.secondary)

                Picker("Speech locale", selection: Binding(
                    get: { preferences.speechLocale },
                    set: { preferences.speechLocale = $0; restart() })) {
                    ForEach(locales, id: \.self) { locale in
                        Text(Self.localeLabel(locale)).tag(locale)
                    }
                }
                Text("On-device transcription language")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .formStyle(.grouped)
        .frame(minHeight: 420)
    }

    /// A capture setting only reaches the recorder through its flags, so
    /// changing one replaces the child. The restart goes through the same
    /// graceful stop as everything else — never a kill.
    private func restart() {
        guard recorder.state == .recording else { return }
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
