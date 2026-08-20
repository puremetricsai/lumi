import AppKit
import SwiftUI

/// StorageSettings is the Storage tab: where the store lives, and how large it
/// is on disk.
///
/// It reads the data directory and does nothing else to it. Captured media has
/// exactly two sanctioned deletion paths, `lumi prune` and `lumi compress`, and
/// neither of them is a file operation this app may improvise — so changing the
/// directory changes where *new* capture goes, and the old store is left
/// untouched and named. Moving it is the user's to do in Finder.
struct StorageSettings: View {
    @Environment(RecorderController.self) private var recorder

    /// The path is held here rather than read from Preferences on every draw.
    /// Preferences' values are computed over UserDefaults, so `@Observable`
    /// reports no change when one is written and a view keyed on it would show
    /// a stale path. This copy is the single driver: it feeds the label, the
    /// measurement, and the reveal.
    @State private var dataDirectory = Preferences.shared.dataDirectory
    @State private var notice = StorageRelocationNotice.shared

    @State private var usage: StorageUsage?
    @State private var isMeasuring = false
    @State private var revealProblem: String?

    var body: some View {
        Form {
            Section("Data location") {
                LabeledContent("Data directory") {
                    Text((dataDirectory as NSString).abbreviatingWithTildeInPath)
                        .font(.system(size: 11, design: .monospaced))
                        .textSelection(.enabled)
                        .multilineTextAlignment(.trailing)
                        .fixedSize(horizontal: false, vertical: true)
                }

                HStack {
                    Button("Choose…") { chooseDirectory() }
                        .accessibilityLabel("Choose a data directory")
                    Button("Reveal in Finder") { Task { await revealInFinder() } }
                    Spacer()
                }

                if let revealProblem {
                    SettingsCaption(revealProblem)
                }

                if Preferences.shared.hasCustomDataDirectory {
                    SettingsCaption("You chose this location. A lumi command in Terminal still uses its own "
                        + "default unless you pass --data-dir.")
                } else {
                    SettingsCaption("The default location. The lumi command line resolves the same one, "
                        + "so both read one store.")
                }

                if let previous = notice.previousDirectory {
                    relocationNotice(previous: previous)
                }
            }

            Section {
                StorageBar(segments: segments)
                ForEach(segments) { segment in
                    LabeledContent {
                        Text(segment.detail)
                            .foregroundStyle(.secondary)
                    } label: {
                        HStack(spacing: 8) {
                            RoundedRectangle(cornerRadius: 2)
                                .fill(Theme.accent.opacity(segment.opacity))
                                .frame(width: 10, height: 10)
                                .accessibilityHidden(true)
                            Text(segment.title)
                        }
                    }
                }

                if usage?.directoryExists == false {
                    SettingsCaption("This directory does not exist yet. Lumi creates it when "
                        + "recording starts.")
                }
            } header: {
                HStack(spacing: 8) {
                    Text("Disk usage")
                    Spacer()
                    if isMeasuring {
                        ProgressView()
                            .controlSize(.small)
                            .accessibilityLabel("Measuring disk usage")
                    }
                    Button {
                        Task { await measure(reset: false) }
                    } label: {
                        Image(systemName: "arrow.clockwise")
                    }
                    .buttonStyle(.borderless)
                    .disabled(isMeasuring)
                    .accessibilityLabel("Refresh disk usage")
                    .help("Measure the data directory again")
                }
            }
        }
        .formStyle(.grouped)
        .frame(minHeight: 420)
        // Re-measures when the tab appears and whenever the directory changes.
        // Cancelling this task cancels the walk itself; see measure().
        .task(id: dataDirectory) {
            await measure(reset: true)
        }
    }

    // MARK: - Relocation

    /// The notice shown after the directory changed. Lumi moves no media, so the
    /// old path is named here: it is the only place the user can learn where
    /// their earlier recordings went.
    private func relocationNotice(previous: String) -> some View {
        HStack(alignment: .top, spacing: 8) {
            Image(systemName: "info.circle.fill")
                .foregroundStyle(Theme.attention)
                .accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 4) {
                Text("Your earlier recordings stay where they were:")
                    .font(.callout)
                Text((previous as NSString).abbreviatingWithTildeInPath)
                    .font(.system(size: 11, design: .monospaced))
                    .textSelection(.enabled)
                SettingsCaption("Lumi does not move or copy captured media. New captures and searches "
                    + "use the new directory. Move the old files in Finder if you want one store.")
            }
            Spacer(minLength: 0)
            Button("Dismiss") { notice.previousDirectory = nil }
                .buttonStyle(.borderless)
        }
        .accessibilityElement(children: .contain)
    }

    // MARK: - Actions

    private func chooseDirectory() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.canCreateDirectories = true
        panel.allowsMultipleSelection = false
        panel.prompt = "Use This Folder"
        panel.message = "Choose where Lumi stores screenshots, audio, and its index."
        panel.directoryURL = URL(fileURLWithPath: dataDirectory)
        guard panel.runModal() == .OK, let url = panel.url else { return }

        let chosen = url.path
        let previous = dataDirectory
        // Picking the directory it is already in is not a move: no notice, no
        // restart, and no second walk of the same tree.
        guard chosen != previous else { return }

        Preferences.shared.dataDirectory = chosen
        dataDirectory = chosen
        notice.previousDirectory = previous
        revealProblem = nil
        restart()
    }

    private func revealInFinder() async {
        let path = dataDirectory
        let exists = await Task.detached(priority: .userInitiated) {
            var isDirectory: ObjCBool = false
            let found = FileManager.default.fileExists(atPath: path, isDirectory: &isDirectory)
            return found && isDirectory.boolValue
        }.value
        guard exists else {
            // Opening Finder on a path that is not there gives a window on
            // nothing, which reads as "the store is empty" rather than "the
            // store has not been created".
            revealProblem = "This directory does not exist yet, so there is nothing to reveal. "
                + "Lumi creates it when recording starts."
            return
        }
        revealProblem = nil
        NSWorkspace.shared.selectFile(nil, inFileViewerRootedAtPath: path)
    }

    /// The data directory only reaches the recorder as a flag, so changing it
    /// replaces the child. Same graceful stop as every other setting — never a
    /// kill. This mirrors `RecordingSettings.restart()`.
    private func restart() {
        // isSupervisingRecorder, not `state == .recording`: a permission
        // revoked mid-capture moves the UI to .needsPermissions while the child
        // is still running and still writing. Gating on the UI state there
        // would save the setting and leave the live recorder using the old
        // flags, with nothing to say the two had diverged.
        guard recorder.isSupervisingRecorder else { return }
        Task { await recorder.restart() }
    }

    // MARK: - Measurement

    /// measure walks the data directory off the main actor.
    ///
    /// `reset` clears the previous figures. It is set when the directory may
    /// have changed, because leaving the old store's numbers under a new path
    /// states something false; a manual Refresh keeps them, so the panel does
    /// not empty out while it re-counts.
    private func measure(reset: Bool) async {
        let path = dataDirectory
        if reset { usage = nil }
        isMeasuring = true

        // A store with tens of thousands of frames takes seconds to add up, so
        // the walk is detached. The cancellation handler is what makes the
        // in-loop cancellation check live: nothing else ever cancels a detached
        // task, and without it a closed tab would keep walking.
        let walk = Task.detached(priority: .utility) { StorageUsage.measure(in: path) }
        let measured = await withTaskCancellationHandler {
            await walk.value
        } onCancel: {
            walk.cancel()
        }

        // A cancelled walk returns whatever it had counted so far. That is a
        // partial total and must never be published as a measurement.
        guard !Task.isCancelled else { return }
        usage = measured
        isMeasuring = false
    }

    /// The three legend rows, in the mockup's order, each carrying the colour
    /// its slice of the bar uses.
    private var segments: [StorageSegment] {
        let measuring = "Measuring…"
        let parts: [(title: String, bytes: Int64, detail: String)] = [
            ("Screenshots", usage?.screenshotBytes ?? 0,
             usage.map { "\(Format.bytes($0.screenshotBytes)) · \(Format.count($0.screenshotFiles)) frames" }
                ?? measuring),
            ("Audio (WAV)", usage?.audioBytes ?? 0,
             usage.map { "\(Format.bytes($0.audioBytes)) · \(Format.count($0.audioChunks)) chunks" }
                ?? measuring),
            // FTS5 rather than a file count, per the mockup: the index is three
            // files at most and counting them tells the user nothing.
            ("Index (lumi.db)", usage?.indexBytes ?? 0,
             usage.map { "\(Format.bytes($0.indexBytes)) · FTS5" } ?? measuring),
        ]

        // The largest part gets the full accent; the other two take the lower
        // opacities in the order they are listed above. With nothing measured
        // yet every part is zero, so the first row holds the full accent and the
        // legend keeps its usual look.
        let largest = parts.map(\.bytes).max() ?? 0
        let fullAccent = largest > 0 ? (parts.firstIndex { $0.bytes == largest } ?? 0) : 0
        var lower = [0.62, 0.38]

        return parts.enumerated().map { index, part in
            let opacity: Double
            if index == fullAccent {
                opacity = 1
            } else {
                opacity = lower.isEmpty ? 0.38 : lower.removeFirst()
            }
            return StorageSegment(title: part.title, bytes: part.bytes, detail: part.detail,
                                  opacity: opacity)
        }
    }
}

/// StorageSegment is one legend row and its slice of the bar. They share a value
/// so the swatch beside a number can never be a different colour from the slice
/// that number describes.
struct StorageSegment: Identifiable {
    let title: String
    let bytes: Int64
    let detail: String
    let opacity: Double

    var id: String { title }
}

/// StorageBar is the stacked proportion bar above the legend.
struct StorageBar: View {
    var segments: [StorageSegment]

    private var total: Int64 { segments.reduce(0) { $0 + $1.bytes } }

    var body: some View {
        GeometryReader { proxy in
            HStack(spacing: 0) {
                // An empty store draws the bare track. Three zero-width slivers
                // would claim a measurement nobody made.
                if total > 0 {
                    ForEach(segments) { segment in
                        Theme.accent.opacity(segment.opacity)
                            .frame(width: proxy.size.width * CGFloat(Double(segment.bytes) / Double(total)))
                    }
                }
                Spacer(minLength: 0)
            }
        }
        .frame(height: 10)
        .clipShape(Capsule())
        .background(Capsule().fill(.quaternary))
        .padding(.vertical, 4)
        // The legend below carries every figure this draws, so a second reading
        // of the same numbers is noise.
        .accessibilityHidden(true)
    }
}

/// StorageUsage is one read-only measurement of the data directory.
///
/// It is a plain value so the walk can run off the main actor and hand the whole
/// result back at once.
struct StorageUsage: Sendable {
    var screenshotBytes: Int64 = 0
    var screenshotFiles: Int = 0
    var audioBytes: Int64 = 0
    var audioChunks: Int = 0
    var indexBytes: Int64 = 0
    var directoryExists = false

    /// The index's files, in the layout `internal/config.Paths` derives. The
    /// write-ahead log and the shared-memory file are part of the index on disk;
    /// leaving them out under-reports it during a live recording, which is
    /// exactly when someone opens this tab.
    static let indexFiles = ["lumi.db", "lumi.db-wal", "lumi.db-shm"]

    /// measure adds up the store. It only reads: no file is opened, moved, or
    /// removed, and `lumi.db` is never one of them — the schema belongs to
    /// `internal/store` and this needs a size, not a row.
    static func measure(in root: String) -> StorageUsage {
        var usage = StorageUsage()
        let manager = FileManager.default
        var isDirectory: ObjCBool = false
        usage.directoryExists =
            manager.fileExists(atPath: root, isDirectory: &isDirectory) && isDirectory.boolValue
        guard usage.directoryExists else { return usage }

        let base = URL(fileURLWithPath: root, isDirectory: true)
        let screenshots = walk(base.appendingPathComponent("screenshots", isDirectory: true))
        usage.screenshotBytes = screenshots.bytes
        usage.screenshotFiles = screenshots.files

        let audio = walk(base.appendingPathComponent("audio", isDirectory: true))
        usage.audioBytes = audio.bytes
        usage.audioChunks = audio.files

        for name in indexFiles {
            usage.indexBytes += size(of: base.appendingPathComponent(name))
        }
        return usage
    }

    /// walk totals the regular files under a directory. A directory that is not
    /// there yet counts as empty, which is what it is.
    private static func walk(_ directory: URL) -> (bytes: Int64, files: Int) {
        let keys: [URLResourceKey] = [.fileSizeKey, .isRegularFileKey]
        guard let enumerator = FileManager.default.enumerator(
            at: directory,
            includingPropertiesForKeys: keys,
            options: [.skipsHiddenFiles],
            // One unreadable subdirectory must not abandon the whole total.
            errorHandler: { _, _ in true }) else {
            return (0, 0)
        }
        var bytes: Int64 = 0
        var files = 0
        for case let url as URL in enumerator {
            if Task.isCancelled { break }
            guard let values = try? url.resourceValues(forKeys: Set(keys)),
                  values.isRegularFile == true, let size = values.fileSize else { continue }
            bytes += Int64(size)
            files += 1
        }
        return (bytes, files)
    }

    private static func size(of file: URL) -> Int64 {
        guard let values = try? file.resourceValues(forKeys: [.fileSizeKey, .isRegularFileKey]),
              values.isRegularFile == true, let size = values.fileSize else { return 0 }
        return Int64(size)
    }
}

/// StorageRelocationNotice remembers the directory the store was in before the
/// user pointed Lumi somewhere else.
///
/// It lasts as long as the process and no longer. The notice has to survive
/// switching tabs and closing the Settings window — the user needs the old path
/// long enough to act on it — but it is not a preference, so a relaunch clears
/// it. A stored property is also what makes it observable at all: Preferences'
/// values are computed over UserDefaults and report no changes.
/// It is main-actor isolated because its one instance is a shared mutable
/// global: without that, `shared` is a strict-concurrency error under the Swift
/// 6 language mode. Every reader is a SwiftUI view, which is already on the main
/// actor, so the isolation costs nothing.
@Observable
@MainActor
final class StorageRelocationNotice {
    static let shared = StorageRelocationNotice()

    /// The path the store was in, or nil when nothing moved or the user
    /// dismissed the notice.
    var previousDirectory: String?

    private init() {}
}
