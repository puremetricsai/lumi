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

    /// The connected displays, from `lumi displays --json`. Empty until the
    /// command has answered, and after a failure.
    @State private var displays: [Display] = []
    @State private var loadingDisplays = false
    @State private var displaysError: String?

    /// The choices offered for each duration. The defaults match
    /// `recordFlags.bind` exactly: 2s and 30s.
    private let intervals: [Double] = [1, 2, 5, 10, 30]
    private let chunks: [Double] = [15, 30, 60, 120]
    private let locales = ["en-US", "en-GB", "de-DE", "es-ES", "fr-FR", "it-IT", "ja-JP", "pt-BR", "zh-CN"]

    var body: some View {
        Form {
            // First, and its own section: it is the one control here that is
            // not a capture flag. Everything below restarts the recorder when
            // it changes; this one must not, and never reaches the binary at
            // all.
            Section("Shortcut") {
                ShortcutRecorder()
            }

            Section("Screen") {
                Toggle("Capture screen", isOn: Binding(
                    get: { preferences.captureScreen },
                    set: { preferences.captureScreen = $0; restart() }))
                SettingsCaption("Hot-plug aware: a display plugged in while recording is picked up "
                    + "without restarting capture.")

                displayPicker

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
        // Keyed on whether the picker can show anything at all, so turning
        // Capture screen back on — or granting Screen Recording while this tab
        // is open — takes previews now rather than showing whichever ones
        // happened to be captured when the tab first appeared. The id governs
        // re-running only; the body runs on first appearance either way, which
        // is why `loadDisplays` repeats both conditions as guards.
        .task(id: preferences.captureScreen && recorder.permissions.screenRecording.isGranted) {
            await loadDisplays()
        }
        // A monitor plugged in or pulled out while this tab is open, which no
        // other signal here reports. It matters because the list is read as the
        // *connected* set: a stale one lets unchecking a display that is already
        // gone slip past the last-display guard and store a selection naming
        // nothing connected. Scoped to this view's lifetime and to an event the
        // user caused, so it is not the timer the comment above rules out.
        .task {
            let changes = NotificationCenter.default.notifications(
                named: NSApplication.didChangeScreenParametersNotification)
            for await _ in changes { await loadDisplays() }
        }
    }

    /// The display picker: one row per connected display, each showing what is
    /// on it right now.
    ///
    /// The rows come from `lumi displays --json` — which displays exist, and
    /// what they look like, are both the binary's answers, and Swift calls no
    /// capture framework of its own. Only the name is added here, from
    /// `NSScreen.localizedName`, which is a macOS fact Lumi holds no opinion
    /// about.
    @ViewBuilder
    private var displayPicker: some View {
        if !preferences.captureScreen {
            EmptyView()
        } else if !recorder.permissions.screenRecording.isGranted {
            SettingsCaption("Grant Screen Recording to choose displays and see what is on each. "
                + "Until then Lumi records every connected display.")
        } else {
            ForEach(displays) { display in
                Toggle(isOn: binding(for: display)) {
                    HStack(spacing: 10) {
                        thumbnail(for: display)
                        VStack(alignment: .leading, spacing: 2) {
                            Text(display.name)
                            Text(display.resolution.map { "\($0) · ID \(display.displayId)" }
                                ?? "ID \(display.displayId)")
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        }
                    }
                }
                // Deselecting the last display would be "record the screen, but
                // no screens", which is what the Capture screen toggle above is
                // for. Never disabled while off, so a selection can always grow.
                .disabled(isOnlySelected(display))
            }
            HStack {
                if let displaysError {
                    Text(displaysError).font(.caption).foregroundStyle(.secondary)
                } else {
                    SettingsCaption(selectionCaption)
                }
                Spacer()
                Button("Refresh") { Task { await loadDisplays() } }
                    .disabled(loadingDisplays)
            }
        }
    }

    private var selectionCaption: String {
        let selected = preferences.selectedDisplayIDs
        if selected.isEmpty { return "Recording every connected display." }
        let missing = selected.filter { id in !displays.contains { $0.displayId == id } }
        // A fact about two lists, and it stops there: what the recorder does
        // when a selection cannot be honoured is the recorder's to say, and it
        // does — on the wire, as `selection_fallback`, which the display pill
        // reads. A second telling here would be the same rule in two languages.
        if missing.count == selected.count {
            return "None of the selected displays are connected."
        }
        if !missing.isEmpty {
            return "\(missing.count) selected display(s) are not connected. They stay selected, "
                + "so reconnecting one resumes recording it."
        }
        return "Previews are taken when this tab opens. Refresh to take them again."
    }

    @ViewBuilder
    private func thumbnail(for display: Display) -> some View {
        if let image = display.thumbnail {
            Image(nsImage: image)
                .resizable()
                .aspectRatio(contentMode: .fit)
                .frame(width: 96)
                .clipShape(RoundedRectangle(cornerRadius: 4))
                .overlay(RoundedRectangle(cornerRadius: 4).strokeBorder(.separator))
        } else {
            RoundedRectangle(cornerRadius: 4)
                .fill(.quaternary)
                .frame(width: 96, height: 54)
                .overlay(Image(systemName: "display.trianglebadge.exclamationmark"))
                .help(display.captureError ?? "No preview")
        }
    }

    /// An empty stored selection means "every display", so the toggles read as
    /// all-on until the first one is turned off — at which point the selection
    /// becomes every *other* connected display. Unchecking one display must not
    /// silently deselect the ones that are merely unplugged.
    private func binding(for display: Display) -> Binding<Bool> {
        Binding(
            get: {
                let selected = preferences.selectedDisplayIDs
                return selected.isEmpty || selected.contains(display.displayId)
            },
            set: { isOn in
                var selected = preferences.selectedDisplayIDs
                if selected.isEmpty { selected = displays.map(\.displayId) }
                if isOn {
                    if !selected.contains(display.displayId) { selected.append(display.displayId) }
                } else {
                    selected.removeAll { $0 == display.displayId }
                }
                // Every connected display checked is stored as no selection at
                // all, which is what it means. Storing the list instead would
                // silently exclude a display plugged in later — nobody chose
                // that. Safe for the unplugged: a stored ID that is not
                // connected makes the two sets differ, so the list is kept.
                if Set(selected) == Set(displays.map(\.displayId)) { selected = [] }
                preferences.selectedDisplayIDs = selected
                restart()
            })
    }

    /// Whether this is the last *connected* display still checked.
    ///
    /// Counted over what is connected and checked, not over the stored list: a
    /// stored selection may name displays that are unplugged, and comparing
    /// against the whole list would leave the last connected one unchecked —
    /// "record the screen, but no screens", which is what the Capture screen
    /// toggle above is for.
    private func isOnlySelected(_ display: Display) -> Bool {
        let checked = displays.filter { binding(for: $0).wrappedValue }
        return checked.count == 1 && checked.first?.displayId == display.displayId
    }

    /// loadDisplays asks the binary what is connected.
    ///
    /// Called when the picker becomes showable, when the displays change, and
    /// from Refresh, never on a timer: each call is a real screen capture, and a
    /// polling preview would be a second capture pipeline running beside the
    /// recorder.
    ///
    /// Both of `displayPicker`'s branches are guards here, not just the Screen
    /// Recording one. `.task(id:)` runs its body on first appearance whatever
    /// the id evaluates to — the id only decides when it runs *again* — so a
    /// guard that omits `captureScreen` means opening Settings screenshots every
    /// display for a picker that renders `EmptyView()`, after the user has said
    /// not to capture the screen. The grant is a guard for the matching reason:
    /// opening Settings must never be what raises a TCC prompt.
    private func loadDisplays() async {
        guard preferences.captureScreen, recorder.permissions.screenRecording.isGranted,
              !loadingDisplays else { return }
        loadingDisplays = true
        defer { loadingDisplays = false }
        do {
            displays = try await LumiCLI.json([Display].self, ["displays", "--json"])
            displaysError = nil
        } catch {
            // Emptied, not left standing. `displays` is read as the *connected*
            // set by the selection canonicalisation and by `isOnlySelected`, so
            // a stale list is not a cosmetic problem: unchecking a display that
            // is no longer connected would slip past the last-display guard and
            // store a selection naming nothing connected, which the recorder
            // answers by recording every display — the opposite of what was
            // asked, and it outlives the app.
            displays = []
            displaysError = error.localizedDescription
        }
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
