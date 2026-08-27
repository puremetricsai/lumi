import Foundation

/// UpdateChecker asks the bundled binary whether a newer Lumi exists, and hands
/// it the upgrade when the user takes it.
///
/// It holds no URL, no version comparison, and no knowledge of how an upgrade is
/// performed. `internal/cli` owns all three: `lumi update --json` answers what is
/// available and `lumi update --apply` runs install.sh, which is the one install
/// channel and stays the one install channel. Whether to *ask* is the only part
/// of this the app decides, and that lives in `Preferences`.
@Observable
@MainActor
final class UpdateChecker {
    /// The last answer the binary gave, or nil before the first check returns.
    private(set) var status: UpdateStatus? {
        didSet { statusDidChange?() }
    }

    /// Why the last check the *user asked for* failed. An automatic check
    /// clears this rather than setting it: an offline laptop must not leave an
    /// error in a tab nobody opened, but a button somebody pressed owes them an
    /// answer.
    private(set) var lastError: String?

    private(set) var isChecking = false

    /// AppKit's menu is not part of SwiftUI's observation tree, so it uses this
    /// hook to redraw when an update appears. Same shape as
    /// `RecorderController.statusDidChange`.
    var statusDidChange: (() -> Void)?

    /// How often the app asks on its own. One request a day is enough to notice
    /// a release; anything faster would be traffic that buys nothing.
    static let automaticInterval: Duration = .seconds(24 * 60 * 60)

    /// check asks the binary. `userInitiated` decides only what happens to a
    /// failure, never whether the request is made.
    func check(userInitiated: Bool = false) async {
        guard !isChecking else { return }
        isChecking = true
        defer { isChecking = false }
        do {
            // No --data-dir: asking about versions must not depend on a store
            // existing or being readable, same as the version row above it.
            status = try await LumiCLI.json(
                UpdateStatus.self, ["update", "--json"], includeDataDirectory: false)
            lastError = nil
        } catch {
            if userInitiated { lastError = error.localizedDescription }
        }
    }

    /// apply starts the installer and returns once it is running, not once it
    /// has finished — it deliberately outlives this process. The caller quits
    /// the app itself afterwards.
    ///
    /// Throws so the caller can leave the app running and show why. `LumiCLI.run`
    /// does not throw on a non-zero exit, so the status is checked here.
    func apply() async throws {
        let result = try await LumiCLI.run(["update", "--apply"])
        guard result.succeeded else {
            throw LumiCLI.Failure.command(
                arguments: ["update", "--apply"], status: result.status, stderr: result.stderr)
        }
    }

    /// startAutomaticChecks runs one check now and one a day after, for as long
    /// as the app lives.
    ///
    /// A `Task.sleep` loop rather than a `Timer`: it cancels with the task, and
    /// it is the same shape `RecorderController` already uses for its poll. The
    /// preference is read each time round rather than captured, so switching it
    /// off takes effect without a relaunch.
    func startAutomaticChecks() {
        Task { [weak self] in
            while !Task.isCancelled {
                if Preferences.shared.checkForUpdates {
                    await self?.check()
                }
                do {
                    try await Task.sleep(for: Self.automaticInterval)
                } catch {
                    return
                }
            }
        }
    }
}
