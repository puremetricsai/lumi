import SwiftUI

/// DangerSettings is the Danger tab: the two ways to throw captured data away.
///
/// Nothing here deletes a row or a file. Both actions shell out to `lumi prune`
/// and show its `--json` result. Row-before-file ordering is
/// `internal/retention`'s rule and stays there — a second deletion path written
/// in Swift is exactly the drift the repository's `CLAUDE.md` forbids.
struct DangerSettings: View {
    @Environment(RecorderController.self) private var recorder

    /// The units offered for the age field.
    ///
    /// Hours and minutes only. `--older-than` is parsed by `time.ParseDuration`,
    /// and a Go duration has no `d` unit, so a "days" choice would build a string
    /// the CLI rejects — and the user would see a parse error that names a flag
    /// they never typed. The raw value is the suffix the flag receives.
    enum AgeUnit: String, CaseIterable, Identifiable {
        case hours = "h"
        case minutes = "m"

        var id: String { rawValue }
        var label: String { self == .hours ? "hours" : "minutes" }
    }

    /// 720h is 30 days — the example the CLI's own help gives.
    private static let defaultAmount = "720"

    private enum Action: String, Identifiable {
        case prune
        case deleteAll

        var id: String { rawValue }
    }

    /// What the last run of one action reported. It is kept until the user
    /// changes an input, so the numbers stay readable after the sheet closes.
    private struct Report {
        var action: Action
        var isPreview: Bool
        var result: PruneResult? = nil
        var failure: String? = nil
    }

    /// The age is held as text, not as a number. A number field cannot express
    /// "the user cleared it", and an empty or zero age would build
    /// `--older-than 0h`, which means "everything older than now" — a full wipe
    /// through the path that asks for no typed confirmation.
    @State private var amountText = DangerSettings.defaultAmount
    @State private var unit: AgeUnit = .hours
    @State private var confirming: Action?
    @State private var confirmWord = ""
    @State private var runningAction: Action?
    @State private var report: Report?

    /// The word the destructive action asks the user to type.
    private static let confirmationWord = "delete"

    var body: some View {
        Form {
            Section {
                banner
            }

            Section("Prune older data") {
                LabeledContent("Delete media older than") {
                    HStack(spacing: 8) {
                        TextField(DangerSettings.defaultAmount, text: $amountText)
                            .frame(width: 72)
                            .multilineTextAlignment(.trailing)
                            .accessibilityLabel("Age")
                        Picker("Unit", selection: $unit) {
                            ForEach(AgeUnit.allCases) { option in
                                Text(option.label).tag(option)
                            }
                        }
                        .labelsHidden()
                        .frame(width: 108)
                        .accessibilityLabel("Age unit")
                    }
                }
                SettingsCaption("Removes screenshots and audio captured before this age.")

                // The exact duration is shown because it is what the flag
                // receives. Seeing "720h" is what stops a user asking for "30d".
                if let duration = olderThanDuration {
                    SettingsCaption("Runs lumi prune --older-than \(duration)")
                } else {
                    Text("Enter a whole number of \(unit.label), 1 or more.")
                        .font(.caption)
                        .foregroundStyle(Theme.recording)
                }

                HStack(spacing: 10) {
                    Button("Preview") { run(.prune, preview: true) }
                        .disabled(olderThanDuration == nil || runningAction != nil)
                        .accessibilityLabel("Preview what pruning would delete")
                    Button("Prune") { present(.prune) }
                        .disabled(olderThanDuration == nil || runningAction != nil)
                    Spacer()
                    Button("Reset") { reset() }
                        .disabled(runningAction != nil)
                        .accessibilityLabel("Reset the age to its default")
                }
                reportView(for: .prune)
            }

            Section("Delete all data") {
                SettingsCaption("Erases every screenshot, audio chunk, and the index.")
                if isCapturing {
                    SettingsCaption("Stop recording first. This is the one action that also sweeps "
                        + "media no event points at yet, and a running recorder is writing exactly "
                        + "that: a file saved a moment before the sweep is indexed a moment after "
                        + "it, which leaves a row naming media that no longer exists. Use Stop "
                        + "recording in the Lumi window.")
                }
                HStack(spacing: 10) {
                    Button("Preview") { run(.deleteAll, preview: true) }
                        .disabled(runningAction != nil)
                        .accessibilityLabel("Preview what deleting all data would remove")
                    Button("Delete all data…", role: .destructive) { present(.deleteAll) }
                        .buttonStyle(.bordered)
                        .tint(Theme.recording)
                        // Blocked while the app holds a recorder, not merely
                        // discouraged. `prune --all` sweeps the media
                        // directories as well as the indexed rows, so it races
                        // a live recorder in the one direction the repository
                        // forbids — a row that names a file which is gone.
                        // Deleting is recoverable in neither direction, so the
                        // app refuses rather than stopping capture on the
                        // user's behalf. Preview is left enabled: it deletes
                        // nothing.
                        .disabled(runningAction != nil || isCapturing)
                        .help(isCapturing
                              ? "Stop recording before deleting all data"
                              : "Delete every event and all media")
                    Spacer()
                }
                reportView(for: .deleteAll)
            }
        }
        .formStyle(.grouped)
        .frame(minHeight: 420)
        // `recorder.status` is only polled while this app supervises a
        // recorder, so a recorder someone started in a terminal would read as
        // absent here. Re-read it when the tab appears, so the wipe is blocked
        // on what is actually running rather than on what the app happens to
        // own.
        .task { await recorder.refreshStatus() }
        // A result that outlives its inputs is a lie: "would delete 3 events"
        // must not sit under a field the user has since changed.
        .onChange(of: amountText) { _, _ in report = nil }
        .onChange(of: unit) { _, _ in report = nil }
        .sheet(item: $confirming) { action in
            confirmationSheet(action)
        }
    }

    // MARK: - Banner

    private var banner: some View {
        HStack(alignment: .firstTextBaseline, spacing: 9) {
            Image(systemName: "exclamationmark.triangle.fill")
                .foregroundStyle(Theme.recording)
            Text("These actions permanently delete captured media and cannot be undone.")
                .font(.callout)
                .fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 0)
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 10)
        // The banner is the one red field on the tab, and it carries its own
        // fill rather than handing it to `listRowBackground`. A macOS grouped
        // Form is not a List, so that row modifier has nothing to colour and
        // the banner would come out plain — with no error anywhere to say so.
        // The tint is the one StatePill and Badge use, so it reads as this
        // app's red rather than a second one.
        .background(RoundedRectangle(cornerRadius: 8).fill(Theme.recording.opacity(0.14)))
        .accessibilityElement(children: .combine)
    }

    // MARK: - Result

    @ViewBuilder
    private func reportView(for action: Action) -> some View {
        if runningAction == action {
            HStack(spacing: 8) {
                ProgressView()
                    .controlSize(.small)
                Text(action == .deleteAll ? "Working through every event…" : "Working through the store…")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        } else if let report, report.action == action {
            if let failure = report.failure {
                Text(failure)
                    .font(.caption)
                    .foregroundStyle(Theme.recording)
                    .fixedSize(horizontal: false, vertical: true)
            } else if let result = report.result {
                VStack(alignment: .leading, spacing: 3) {
                    Text(headline(for: result, isPreview: report.isPreview))
                        .font(.callout)
                    // Both counts are only shown when they are not zero: a
                    // steady "0 files no event referenced" reads as a fault
                    // report on a store that is perfectly healthy.
                    if result.missingFiles > 0 {
                        SettingsCaption("\(Format.count(result.missingFiles)) events referenced media that was already gone.")
                    }
                    if result.orphanFiles > 0 {
                        SettingsCaption("\(Format.count(result.orphanFiles)) files no event referenced. Their bytes are counted above.")
                    }
                }
                .accessibilityElement(children: .combine)
            }
        }
    }

    private func headline(for result: PruneResult, isPreview: Bool) -> String {
        let events = Format.count(Int(result.events))
        let bytes = Format.bytes(result.bytes)
        if isPreview {
            return "Would delete \(events) events, \(bytes)."
        }
        return "Deleted \(events) events, freed \(bytes)."
    }

    // MARK: - Confirmation

    @ViewBuilder
    private func confirmationSheet(_ action: Action) -> some View {
        VStack(alignment: .leading, spacing: 14) {
            HStack(spacing: 9) {
                Image(systemName: "exclamationmark.triangle.fill")
                    .foregroundStyle(Theme.recording)
                Text(action == .deleteAll ? "Delete all data?" : "Prune older data?")
                    .font(.headline)
            }

            Text(sheetMessage(action))
                .fixedSize(horizontal: false, vertical: true)

            // The most recent preview, when it is this action's own. It answers
            // "how much" with a measurement rather than with a guess.
            if let report, report.action == action, report.isPreview, let result = report.result {
                SettingsCaption("The last preview reported \(Format.count(Int(result.events))) events, \(Format.bytes(result.bytes)).")
            }

            if action == .deleteAll {
                // Captured media exists on disk before its event row does — the
                // recorder writes a frame, compares it, then inserts — and the
                // --all sweep removes files no row references. So a frame from
                // the last few seconds goes too. Age-based pruning never
                // sweeps, and needs no such note.
                if recorder.state == .recording {
                    SettingsCaption("Lumi is recording. Media captured a moment ago, but not yet indexed, is deleted as well.")
                }

                VStack(alignment: .leading, spacing: 5) {
                    Text("Type \(DangerSettings.confirmationWord) to confirm.")
                        .font(.callout)
                    TextField(DangerSettings.confirmationWord, text: $confirmWord)
                        .textFieldStyle(.roundedBorder)
                        .accessibilityLabel("Confirmation word")
                }
            }

            HStack {
                Spacer()
                Button("Cancel", role: .cancel) { confirming = nil }
                    .keyboardShortcut(.cancelAction)
                Button(action == .deleteAll ? "Delete Everything" : "Prune", role: .destructive) {
                    confirming = nil
                    run(action, preview: false)
                }
                .buttonStyle(.borderedProminent)
                .tint(Theme.recording)
                .disabled(action == .deleteAll && !confirmationTyped)
            }
        }
        .padding(20)
        .frame(width: 400)
    }

    private func sheetMessage(_ action: Action) -> String {
        switch action {
        case .prune:
            let duration = olderThanDuration ?? ""
            return "Every screenshot and audio chunk captured more than \(duration) ago is deleted, "
                + "with the events that index it. This cannot be undone."
        case .deleteAll:
            return "Every screenshot, every audio chunk, and the whole index are deleted. "
                + "This cannot be undone."
        }
    }

    private var confirmationTyped: Bool {
        confirmWord.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
            == DangerSettings.confirmationWord
    }

    // MARK: - Actions

    /// The `--older-than` value, or nil when the field cannot make one.
    ///
    /// Zero and negative ages are refused rather than clamped. `--older-than 0h`
    /// resolves to "older than now", which deletes the whole store through the
    /// path that asks for no typed confirmation.
    private var olderThanDuration: String? {
        let trimmed = amountText.trimmingCharacters(in: .whitespaces)
        guard let amount = Int(trimmed), amount >= 1 else { return nil }
        return "\(amount)\(unit.rawValue)"
    }

    private func present(_ action: Action) {
        confirmWord = ""
        confirming = action
    }

    /// Reset only puts the age back to its default. It deletes nothing.
    private func reset() {
        amountText = DangerSettings.defaultAmount
        unit = .hours
    }

    /// isCapturing reports whether anything is writing to this store.
    ///
    /// Both sources are consulted. `isSupervisingRecorder` is the child this
    /// app holds; `status.recording` is `lumi record status`, which also sees a
    /// recorder someone started in a terminal. A wipe races either one exactly
    /// the same way, and the app owns only the first.
    private var isCapturing: Bool {
        recorder.isSupervisingRecorder || recorder.status.recording
    }

    private func run(_ action: Action, preview: Bool) {
        guard runningAction == nil else { return }
        // Re-checked here, not only on the button. Capture can start while the
        // confirmation sheet is open, and the sheet's own disabled state was
        // decided before that.
        if action == .deleteAll && !preview && isCapturing { return }

        var arguments: [String]
        switch action {
        case .prune:
            guard let duration = olderThanDuration else { return }
            arguments = ["prune", "--older-than", duration, "--json"]
        case .deleteAll:
            // --yes is not optional here. Without it `lumi prune --all` reads a
            // confirmation line from stdin, and this app gives every child
            // FileHandle.nullDevice, so the command would fail at that read
            // rather than wait. The sheet above is the confirmation.
            arguments = ["prune", "--all", "--yes", "--json"]
        }
        if preview {
            arguments.append("--dry-run")
        }

        runningAction = action
        report = nil
        // A prune over a large store takes time, so it runs off the main thread
        // in LumiCLI and this view only observes the result.
        Task {
            do {
                let result = try await LumiCLI.json(PruneResult.self, arguments)
                report = Report(action: action, isPreview: preview, result: result)
            } catch {
                report = Report(action: action, isPreview: preview, failure: error.localizedDescription)
            }
            runningAction = nil
        }
    }
}
