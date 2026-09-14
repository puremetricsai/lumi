import AppKit
import Foundation
import Observation

/// RecorderController supervises the capture process.
///
/// It holds `lumi record start --foreground` as a directly-held child, and never
/// uses the detaching default. A detached recorder re-execs into its own
/// session, which risks launchd becoming the TCC "responsible process" instead
/// of this bundle — and an app bundle's stable TCC identity is the entire reason
/// this app exists. The measurement behind that is
/// docs/research/2026-08-17-tcc-spike.md.
///
/// Nothing here touches the capture pipeline itself. Display hot-plug, media
/// preservation, retries, and graceful shutdown are all the Go recorder's, and
/// this must not second-guess any of them — in particular it must never restart
/// the recorder because displays changed.
@Observable
@MainActor
final class RecorderController {
    enum State: Equatable {
        case idle
        case recording
        case needsPermissions
    }

    private(set) var state: State = .idle {
        didSet { if state != oldValue { statusDidChange?() } }
    }
    private(set) var permissions = Permissions()
    private(set) var status: RecordStatus = .notRecording
    /// The most recent screen tick, or nil when none has arrived or the last
    /// one went stale.
    ///
    /// Presence *is* freshness here, exactly as it is for `levels`: the poll
    /// prunes a stale report rather than every reader re-testing the clock. Nil
    /// therefore means "capture is not reporting", which the UI must draw as
    /// missing — never as a count borrowed from `NSScreen`, which answers how
    /// many displays exist rather than how many Lumi is recording.
    private(set) var screenCapture: ScreenCapture?
    private(set) var lastError: String?

    /// The most recent measurement per source, keyed by the recorder's own
    /// track names ("system", "microphone").
    private(set) var levels: [String: AudioLevel] = [:]

    /// True while a stop is in flight, so the UI can stay honest during the
    /// graceful-shutdown window rather than claiming the recorder is already
    /// gone.
    private(set) var isStopping = false {
        didSet { if isStopping != oldValue { statusDidChange?() } }
    }

    /// AppKit's menu is not part of SwiftUI's observation tree, so it uses this
    /// hook to redraw immediately when the recorder's visible status changes.
    var statusDidChange: (() -> Void)?

    private var process: Process?
    private var stderrPipe: Pipe?
    private var stderrBuffer = Data()
    private var pollTask: Task<Void, Never>?
    private var permissionPollTask: Task<Void, Never>?

    /// The graceful-stop budget. It is `stopTimeout` from
    /// `internal/cli/record_daemon.go`, which is sized to exceed the recorder's
    /// 5s media-preservation window so in-flight media finishes indexing. It is
    /// duplicated here only because a Swift process cannot read a Go constant;
    /// if that constant moves, this must move with it.
    static let stopTimeout: TimeInterval = 20

    // MARK: - Lifecycle

    func start() async {
        await refreshPermissions()
        guard state != .needsPermissions else { return }
        guard process == nil else { return }
        lastError = nil

        let child = Process()
        child.executableURL = LumiCLI.executableURL
        child.arguments = Preferences.shared.recorderArguments()
        child.standardInput = FileHandle.nullDevice
        // stdout is left alone; the recorder writes its diagnostics and its
        // level lines to stderr.
        let errPipe = Pipe()
        child.standardError = errPipe
        child.standardOutput = FileHandle.nullDevice

        errPipe.fileHandleForReading.readabilityHandler = { [weak self] handle in
            let chunk = handle.availableData
            guard !chunk.isEmpty else { return }
            Task { @MainActor [weak self] in self?.consume(chunk) }
        }
        child.terminationHandler = { [weak self] finished in
            // The exited child is named rather than assumed. The handler runs on
            // a background queue and hops to the main actor, so a restart can
            // start a replacement before the previous child's handler arrives —
            // and an unconditional `process = nil` there would disown the live
            // recorder, leaving the app supervising nothing while capture ran on.
            Task { @MainActor [weak self] in
                self?.recorderExited(finished, status: finished.terminationStatus)
            }
        }

        do {
            try child.run()
        } catch {
            lastError = "Could not start the recorder: \(error.localizedDescription)"
            state = .idle
            return
        }
        process = child
        stderrPipe = errPipe
        state = .recording
        startPolling()
    }

    /// stop ends capture the way `lumi record stop` does: SIGTERM, then wait.
    ///
    /// Never SIGKILL, and never a shorter wait. The recorder holds media that
    /// has been written but not yet indexed, and killing it inside that window
    /// is the one thing the repository's "never lose captured media" rule
    /// forbids outright.
    func stop() async {
        guard let child = process, child.isRunning else {
            process = nil
            state = .idle
            return
        }
        isStopping = true
        stopFailed = false
        defer { isStopping = false }

        child.terminate()
        let deadline = Date().addingTimeInterval(Self.stopTimeout)
        while child.isRunning && Date() < deadline {
            try? await Task.sleep(for: .milliseconds(100))
        }
        if child.isRunning {
            // Reported rather than escalated. Killing it here would discard the
            // in-flight media the wait exists to protect, so the honest outcome
            // is to say the recorder did not stop and leave it running.
            stopFailed = true
            lastError = "The recorder did not stop within \(Int(Self.stopTimeout))s; it is still running. "
                + "Run `lumi record stop` to finish shutting it down."
        }
    }

    /// restart applies a settings change by replacing the recorder, going
    /// through the same graceful stop as everything else.
    func restart() async {
        await stop()
        await start()
    }

    private func recorderExited(_ exited: Process, status: Int32) {
        guard exited === process else { return }
        process = nil
        // Released explicitly rather than left to deallocation: the handler
        // holds a dispatch source on the read end, and letting a restart drop
        // it implicitly leaks the source and the descriptor with it.
        stderrPipe?.fileHandleForReading.readabilityHandler = nil
        try? stderrPipe?.fileHandleForReading.close()
        stderrPipe = nil
        stderrBuffer.removeAll(keepingCapacity: false)
        levels.removeAll()
        // Alongside the levels, and for the same reason: a report from the child
        // that has just exited is not a measurement of the one replacing it.
        // Waiting for the poll to prune it would show the previous recorder's
        // display count as live across a restart — which is exactly what
        // changing a display selection does.
        screenCapture = nil
        pollTask?.cancel()
        pollTask = nil
        if state == .recording {
            state = .idle
        }
        // 0 is a clean stop, and SIGTERM is how a clean stop is requested, so
        // neither is an error worth showing.
        if status != 0 && status != 143 && !isStopping {
            lastError = "The recorder exited unexpectedly (status \(status))."
        }
        // Fired unconditionally: the child being gone is a visible status
        // change on its own, and the assignment above does not always make one.
        // A permission revoked mid-capture leaves `state` at
        // `.needsPermissions` through the exit, so a menu titled on whether a
        // child is held would keep offering to stop one that had already died.
        statusDidChange?()
        Task { await refreshPermissions() }
    }

    // MARK: - Reading the recorder's stderr

    /// consume accumulates stderr and parses whole lines out of it.
    ///
    /// Reads arrive on arbitrary buffer boundaries, so a line is only parsed
    /// once its newline has been seen — a half-written JSON object is
    /// unparseable, not merely out of order. Lines that are not level events
    /// are the recorder's ordinary slog output and are ignored here.
    private func consume(_ chunk: Data) {
        stderrBuffer.append(chunk)
        while let newline = stderrBuffer.firstIndex(of: UInt8(ascii: "\n")) {
            let line = stderrBuffer[stderrBuffer.startIndex..<newline]
            stderrBuffer.removeSubrange(stderrBuffer.startIndex...newline)
            if let level = Self.parseLevel(line) {
                levels[level.source] = level
                continue
            }
            if let tick = Self.parseScreenCapture(line) {
                screenCapture = tick
            }
        }
        // A pathological line with no newline must not grow without bound.
        if stderrBuffer.count > 1 << 20 {
            stderrBuffer.removeAll(keepingCapacity: false)
        }
    }

    static func parseLevel(_ line: Data) -> AudioLevel? {
        guard line.first == UInt8(ascii: "{") else { return nil }
        guard let level = try? LumiCLI.decoder.decode(AudioLevel.self, from: Data(line)) else {
            return nil
        }
        return level.event == "audio_level" ? level : nil
    }

    static func parseScreenCapture(_ line: Data) -> ScreenCapture? {
        guard line.first == UInt8(ascii: "{") else { return nil }
        guard let tick = try? LumiCLI.decoder.decode(ScreenCapture.self, from: Data(line)) else {
            return nil
        }
        return tick.event == "screen_capture" ? tick : nil
    }

    // MARK: - Status and permissions

    private func startPolling() {
        pollTask?.cancel()
        pollTask = Task { [weak self] in
            while !Task.isCancelled {
                await self?.refreshStatus()
                try? await Task.sleep(for: .seconds(5))
            }
        }
    }

    /// refreshStatus asks the CLI, rather than reading `record.json`. The
    /// recorder the app supervises registers itself there under
    /// `--register-state`, so `record status` sees it exactly as it sees a
    /// recorder started from a terminal.
    func refreshStatus() async {
        dropStaleLevels()
        guard let fresh = try? await LumiCLI.json(RecordStatus.self, ["record", "status", "--json"]) else {
            return
        }
        status = fresh
    }

    /// refreshPermissions re-reads TCC and moves the app into or out of the
    /// permissions state. Called on launch and on activation, so a grant made in
    /// System Settings clears without a relaunch.
    func refreshPermissions() async {
        guard let fresh = try? await LumiCLI.json(
            Permissions.self, ["permissions", "--json"], includeDataDirectory: false) else {
            return
        }
        permissions = fresh
        let missing = fresh.missingFor(
            screen: Preferences.shared.captureScreen, audio: Preferences.shared.captureAudio)
        let wasBlocked = state == .needsPermissions
        if !missing.isEmpty {
            state = .needsPermissions
        } else if wasBlocked {
            state = process == nil ? .idle : .recording
        }
        updatePermissionPolling()
        // Recording is the normal state, so a grant that unblocks capture
        // starts it. Making the user click again after they have already said
        // yes in System Settings is a start/stop ceremony this app does not
        // have.
        if wasBlocked && missing.isEmpty && process == nil {
            await start()
        }
    }

    /// updatePermissionPolling re-reads TCC on a timer, but only while the app
    /// is actually blocked on it.
    ///
    /// Activation alone is not enough. Lumi is an accessory app with no Dock
    /// icon, so granting a permission in System Settings and returning to work
    /// never activates it — the window would sit on "Required" indefinitely for
    /// a permission the user had already given. The poll stops the moment
    /// nothing is missing, so the steady state costs nothing.
    private func updatePermissionPolling() {
        guard state == .needsPermissions else {
            permissionPollTask?.cancel()
            permissionPollTask = nil
            return
        }
        guard permissionPollTask == nil else { return }
        permissionPollTask = Task { [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(2))
                guard !Task.isCancelled else { return }
                await self?.refreshPermissions()
            }
        }
    }

    /// requestPermissions runs the native request flows as a child of this
    /// bundle, which is what makes the prompts name Lumi.
    func requestPermissions() async {
        do {
            // `permissions --request` exits non-zero when something is still
            // ungranted after the flows run, which is the ordinary outcome for
            // Screen Recording and Accessibility. That is not a failure to
            // report — only a binary that could not run at all is.
            _ = try await LumiCLI.run(["permissions", "--request"], includeDataDirectory: false)
            lastError = nil
        } catch let failure as LumiCLI.Failure {
            if case .launch = failure {
                lastError = failure.errorDescription
            }
        } catch {
            lastError = error.localizedDescription
        }
        await refreshPermissions()
    }

    /// missingPermissions is the required-but-ungranted set for the current
    /// capture mode, read from the rule rather than counted.
    var missingPermissions: [PermissionRow] {
        permissions.missingFor(
            screen: Preferences.shared.captureScreen, audio: Preferences.shared.captureAudio)
    }

    // MARK: - Meter input

    func level(for source: String) -> AudioLevel? { levels[source] }

    /// isSupervisingRecorder reports whether a child process is actually held.
    ///
    /// Termination must be decided on this rather than on `state`: a permission
    /// revoked mid-capture moves the UI to `.needsPermissions` while the
    /// recorder is still running, and quitting on the UI state would skip the
    /// graceful stop and orphan a live capture.
    var isSupervisingRecorder: Bool { process?.isRunning == true }

    /// canToggleRecording reports whether starting or stopping would do
    /// anything right now.
    ///
    /// One expression, read by both the menu bar's Start/Stop item and the
    /// global shortcut. Two copies of this gate would be free to disagree, and
    /// nothing in either would say which one was wrong.
    var canToggleRecording: Bool {
        !isStopping && !(state == .needsPermissions && !isSupervisingRecorder)
    }

    /// stopFailed reports that the graceful stop timed out and the recorder is
    /// still running. Deliberately not escalated to a kill: the wait exists to
    /// protect media that is written but not yet indexed.
    private(set) var stopFailed = false

    /// dropStaleLevels removes measurements the recorder has stopped refreshing,
    /// so presence in `levels` *is* freshness and no reader has to ask twice.
    ///
    /// It is a prune on a timer rather than a check at draw time because a check
    /// at draw time never runs: freshness turns on wall-clock, `@Observable`
    /// cannot track a clock, and nothing else in the recording card changes
    /// while capture is healthy. Meters therefore held their last height for as
    /// long as the window stayed open — a claim about sound nobody measured,
    /// which is the one thing the meter exists not to do.
    ///
    /// Two chunks is the budget: one missed chunk is tolerated, a second means
    /// the source has stopped reporting.
    private func dropStaleLevels() {
        let now = Date()
        let kept = levels.filter { now.timeIntervalSince($0.value.receivedAt) < TimeInterval($0.value.durationMs) / 1000 * 2 }
        // Guarded because `levels` is not Equatable, so an unconditional assign
        // would invalidate the window every five seconds for nothing.
        if kept.count != levels.count { levels = kept }
        // The screen tick ages by the same rule off its own budget, which is
        // derived from the interval it reports rather than from this app's
        // preference: a recorder started from a terminal sets its own.
        if let tick = screenCapture, now.timeIntervalSince(tick.receivedAt) >= tick.staleAfter {
            screenCapture = nil
        }
    }
}
