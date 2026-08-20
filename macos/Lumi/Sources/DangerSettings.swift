import SwiftUI

/// DangerSettings is the Danger tab: the two ways to throw captured data away,
/// and the one way to rewrite it in place.
///
/// Nothing here deletes a row or a file. The deleting actions shell out to `lumi prune`
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

    private enum Action: String, Identifiable, CaseIterable {
        case prune
        case compress
        case deleteAll

        var id: String { rawValue }
    }

    /// What the last run of one action reported. It is kept until the user
    /// changes an input, so the numbers stay readable after the sheet closes.
    private struct Report {
        var action: Action
        var isPreview: Bool
        var result: PruneResult? = nil
        var compress: CompressResult? = nil
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
    /// What each section would do if it ran, refreshed when the tab appears and
    /// after anything changes the store. A section shows its own real result if
    /// it has one, and this until then.
    @State private var previews: [Action: Report] = [:]
    /// The command each in-flight preview is running. It is the arguments and
    /// not a flag, because that is what decides whether a result that arrives
    /// late still describes what the field now says.
    @State private var previewing: [Action: [String]] = [:]
    /// The pending prune preview. Typing "720" is three edits, and each one
    /// would otherwise walk the whole store.
    @State private var pruneReload: Task<Void, Never>?

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
                        // No title: LabeledContent already labels this row,
                        // and in a Form a TextField's title renders as a
                        // leading label as well as a placeholder.
                        TextField("", text: $amountText)
                            .labelsHidden()
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

                if olderThanDuration == nil {
                    Text("Enter a whole number of \(unit.label), 1 or more.")
                        .font(.caption)
                        .foregroundStyle(Theme.recording)
                }

                HStack(spacing: 10) {
                    Button("Prune") { present(.prune) }
                        .disabled(olderThanDuration == nil || runningAction != nil)
                    Spacer()
                    Button("Reset") { reset() }
                        .disabled(runningAction != nil)
                        .accessibilityLabel("Reset the age to its default")
                }
                reportView(for: .prune)
            }

            Section("Compress media") {
                SettingsCaption("Re-encodes older screenshots as HEIC and older audio as lossless FLAC, "
                    + "in place, then reclaims the database space a prune left behind. No event is deleted.")
                if isCapturing {
                    SettingsCaption("Stop recording first. Compress rewrites the media paths a live "
                        + "recorder is resolving, and its database rebuild wants the store to itself.")
                }
                HStack(spacing: 10) {
                    // Refused while capturing rather than run with
                    // --while-recording. That flag is the CLI's override for
                    // someone who has decided to accept the contention, and
                    // deciding it on the user's behalf is not the app's call.
                    Button("Compress…") { present(.compress) }
                        .disabled(runningAction != nil || isCapturing)
                        .help(isCapturing
                              ? "Stop recording before compressing"
                              : "Re-encode older media in place")
                    Spacer()
                }
                reportView(for: .compress)
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
                        // user's behalf.
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
        .task {
            await recorder.refreshStatus()
            loadPreviews()
        }
        // A result that outlives its inputs is a lie: "would delete 3 events"
        // must not sit under a field the user has since changed. The preview
        // for the new age replaces it, once the typing settles.
        .onChange(of: amountText) { _, _ in pruneInputChanged() }
        .onChange(of: unit) { _, _ in pruneInputChanged() }
        .sheet(item: $confirming) { action in
            confirmationSheet(action)
        }
    }

    // MARK: - Banner

    private var banner: some View {
        HStack(alignment: .firstTextBaseline, spacing: 9) {
            Image(systemName: "exclamationmark.triangle.fill")
                .foregroundStyle(Theme.recording)
            Text("These actions permanently delete or rewrite captured media and cannot be undone.")
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
            spinner(action == .deleteAll ? "Working through every event…" : "Working through the store…")
        } else if previewing[action] != nil {
            spinner("Checking…")
        } else if let report = report?.action == action ? report : previews[action] {
            // Failure and counters are shown together, never one instead of the
            // other: compress commits one file at a time, so a run that failed
            // part way through has real work to report and a real reason it
            // stopped. Showing only the counters would call that run clean.
            VStack(alignment: .leading, spacing: 3) {
                if let failure = report.failure {
                    Text(failure)
                        .font(.caption)
                        .foregroundStyle(Theme.recording)
                        .fixedSize(horizontal: false, vertical: true)
                }
                if let result = report.compress {
                    compressReport(result, isPreview: report.isPreview)
                }
                if let result = report.result {
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
            }
            .accessibilityElement(children: .combine)
        }
    }

    private func spinner(_ text: String) -> some View {
        HStack(spacing: 8) {
            ProgressView()
                .controlSize(.small)
            Text(text)
                .font(.caption)
                .foregroundStyle(.secondary)
        }
    }

    /// The compress counters, in the order a reader asks for them: what moved,
    /// what did not, what an interrupted run left behind, and what the rebuild
    /// gave back.
    @ViewBuilder
    private func compressReport(_ result: CompressResult, isPreview: Bool) -> some View {
        let files = Format.count(Int(result.files))
        if isPreview {
            // No ratio on a preview: measuring one means running the encoder.
            Text("Would compress \(files) files, \(Format.bytes(result.bytesBefore)).")
                .font(.callout)
        } else {
            Text("Compressed \(files) files, \(Format.bytes(result.bytesBefore)) → "
                + "\(Format.bytes(result.bytesAfter)).")
                .font(.callout)
        }
        // Same rule as the prune counters: shown only when they are not zero.
        if result.alreadyDone > 0 {
            SettingsCaption("\(Format.count(Int(result.alreadyDone))) files were already compressed.")
        }
        if result.untouched > 0 {
            SettingsCaption("\(Format.count(Int(result.untouched))) files were left alone. Their originals "
                + "are intact and still indexed; lumi compress says why for each.")
        }
        if result.reconciled.removed > 0 || result.reconciled.recovered > 0 {
            SettingsCaption("Cleaned up after an interrupted earlier run: "
                + "\(Format.count(Int(result.reconciled.removed))) leftover files, "
                + "\(Format.count(Int(result.reconciled.recovered))) events recovered.")
        }
        if result.vacuum.status == "done", let before = result.vacuum.bytesBefore,
           let after = result.vacuum.bytesAfter {
            SettingsCaption("Database rebuilt: \(Format.bytes(before)) → \(Format.bytes(after)).")
        // "skipped: dry run" is not news on a preview — the CLI suppresses the
        // same line for the same reason.
        } else if let detail = result.vacuum.detail, !detail.isEmpty,
                  !(isPreview && result.vacuum.status == "skipped") {
            SettingsCaption("Database not rebuilt: \(detail)")
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
                    .foregroundStyle(action == .compress ? Theme.attention : Theme.recording)
                Text(sheetTitle(action))
                    .font(.headline)
            }

            Text(sheetMessage(action))
                .fixedSize(horizontal: false, vertical: true)

            // The most recent preview, when it is this action's own. It answers
            // "how much" with a measurement rather than with a guess.
            if let result = previews[action]?.result {
                SettingsCaption("The last check reported \(Format.count(Int(result.events))) events, \(Format.bytes(result.bytes)).")
            }
            if let result = previews[action]?.compress {
                SettingsCaption("The last check reported \(Format.count(Int(result.files))) files, \(Format.bytes(result.bytesBefore)).")
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
                Button(confirmLabel(action), role: action == .compress ? nil : .destructive) {
                    confirming = nil
                    run(action)
                }
                .buttonStyle(.borderedProminent)
                .tint(action == .compress ? Theme.accent : Theme.recording)
                .disabled(action == .deleteAll && !confirmationTyped)
            }
        }
        .padding(20)
        .frame(width: 400)
    }

    private func sheetTitle(_ action: Action) -> String {
        switch action {
        case .prune: return "Prune older data?"
        case .compress: return "Compress media?"
        case .deleteAll: return "Delete all data?"
        }
    }

    private func confirmLabel(_ action: Action) -> String {
        switch action {
        case .prune: return "Prune"
        case .compress: return "Compress"
        case .deleteAll: return "Delete Everything"
        }
    }

    private func sheetMessage(_ action: Action) -> String {
        switch action {
        case .prune:
            let duration = olderThanDuration ?? ""
            return "Every screenshot and audio chunk captured more than \(duration) ago is deleted, "
                + "with the events that index it. This cannot be undone."
        case .compress:
            // The lossy half is said plainly. Audio is recoverable sample for
            // sample; a re-encoded screenshot is a second lossy generation on
            // top of the capture JPEG, which docs/compress.md records as a
            // deliberate trade and not an accident.
            return "Older screenshots are re-encoded as HEIC and older audio as lossless FLAC, in "
                + "place. The audio is recoverable sample for sample; the screenshots are a second "
                + "lossy generation and cannot be undone. No event is deleted."
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

    private func run(_ action: Action) {
        guard runningAction == nil, let arguments = arguments(for: action, preview: false) else { return }
        // Re-checked here, not only on the buttons. Capture can start while the
        // confirmation sheet is open, and the sheet's own disabled state was
        // decided before that.
        if action != .prune && isCapturing { return }

        runningAction = action
        report = nil
        // A prune over a large store takes time, so it runs off the main thread
        // in LumiCLI and this view only observes the result.
        Task {
            do {
                report = try await execute(action, arguments: arguments, preview: false)
            } catch {
                report = Report(action: action, isPreview: false, failure: error.localizedDescription)
            }
            runningAction = nil
            // Every section's preview described the store as it was before this
            // ran. Measure them again rather than leave three stale numbers up.
            previews.removeAll()
            loadPreviews()
        }
    }

    /// loadPreviews measures all three sections at once.
    ///
    /// They are separate `lumi` processes deliberately: each dry run writes
    /// nothing, none of them takes the compress lock, and a slow one must not
    /// hold up the other two sections' numbers.
    private func loadPreviews() {
        for action in Action.allCases {
            loadPreview(action)
        }
    }

    private func loadPreview(_ action: Action) {
        guard let arguments = arguments(for: action, preview: true) else { return }
        // A newer run takes the slot rather than being turned away: the age can
        // change while a preview of the old age is still walking the store, and
        // the answer to the question the user is asking now is the one that
        // must arrive. The loser's result is dropped below.
        previewing[action] = arguments
        Task {
            let result: Report
            do {
                result = try await execute(action, arguments: arguments, preview: true)
            } catch {
                result = Report(action: action, isPreview: true, failure: error.localizedDescription)
            }
            // Only the run that still owns the slot may report. A result that
            // outlives its inputs is a lie, and the confirmation sheet quotes
            // this number back before a destructive run.
            guard previewing[action] == arguments else { return }
            previews[action] = result
            previewing[action] = nil
        }
    }

    /// pruneInputChanged drops the old numbers and asks for new ones once the
    /// typing settles. "720" is three edits, and each intermediate age would
    /// otherwise start its own walk of the store.
    private func pruneInputChanged() {
        if report?.action == .prune { report = nil }
        previews[.prune] = nil
        pruneReload?.cancel()
        pruneReload = Task {
            try? await Task.sleep(for: .milliseconds(500))
            guard !Task.isCancelled else { return }
            loadPreview(.prune)
        }
    }

    /// The command line for one action, or nil when the inputs cannot make one.
    private func arguments(for action: Action, preview: Bool) -> [String]? {
        var arguments: [String]
        switch action {
        case .prune:
            guard let duration = olderThanDuration else { return nil }
            arguments = ["prune", "--older-than", duration, "--json"]
        case .deleteAll:
            // --yes is not optional here. Without it `lumi prune --all` reads a
            // confirmation line from stdin, and this app gives every child
            // FileHandle.nullDevice, so the command would fail at that read
            // rather than wait. The sheet above is the confirmation.
            arguments = ["prune", "--all", "--yes", "--json"]
        case .compress:
            // No --older-than and no --while-recording: the CLI's defaults are
            // the contract, and the override belongs to whoever typed it.
            arguments = ["compress", "--json"]
        }
        if preview {
            arguments.append("--dry-run")
        }
        return arguments
    }

    private func execute(_ action: Action, arguments: [String], preview: Bool) async throws -> Report {
        if action == .compress {
            return try await runCompress(arguments, preview: preview)
        }
        let result = try await LumiCLI.json(PruneResult.self, arguments)
        return Report(action: action, isPreview: preview, result: result)
    }

    /// runCompress reads compress's counters *and* its exit status.
    ///
    /// `LumiCLI.json` is not enough here. Compression is committed one file at a
    /// time, so `lumi compress` prints a complete result and then exits non-zero
    /// when the run was cancelled or failed part way through — and a decoder
    /// that stops at the first readable document would render that as a clean
    /// run. Both halves are kept: what was committed, and why it stopped.
    private func runCompress(_ arguments: [String], preview: Bool) async throws -> Report {
        let outcome = try await LumiCLI.run(arguments)
        let data = Data(outcome.stdout.utf8)
        let result = data.isEmpty ? nil : try? LumiCLI.decoder.decode(CompressResult.self, from: data)
        // Nothing readable and nothing to explain it is the one case with no
        // report to make, so it stays an error like any other failed command.
        if result == nil {
            throw LumiCLI.Failure.command(arguments: arguments, status: outcome.status,
                                          stderr: outcome.stderr)
        }
        let failure = outcome.succeeded ? nil
            : LumiCLI.Failure.command(arguments: arguments, status: outcome.status,
                                      stderr: outcome.stderr).localizedDescription
        return Report(action: .compress, isPreview: preview, compress: result, failure: failure)
    }
}
