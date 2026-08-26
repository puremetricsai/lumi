import AppKit
import SwiftUI

/// MCPSettings is the Settings tab for the MCP server.
///
/// Lumi never runs, supervises, or restarts an MCP server. `lumi mcp` is a
/// stdio server that each *client* launches as its own child process, so there
/// is no pid, no port, no uptime, and no log file for this app to show. The
/// design mockup's "Running · pid 48213", "Restart automatically", "Restart
/// server", and the editable argv/env form all describe a daemon Lumi does not
/// have; the layout is kept and every control is re-pointed at what does exist —
/// which clients are registered, and a button that registers them.
///
/// The status itself comes from `lumi mcp setup --dry-run --json`. That is the
/// only read-only status query `internal/mcpsetup` offers: every entry point
/// there writes unless DryRun is set, so the app asks what a run *would* do.
struct MCPSettings: View {
    /// The three things the tab can be showing. A single enum rather than a
    /// report plus two flags, because "loading" and "failed" must not be able
    /// to coexist with a stale report on screen.
    private enum Load {
        case loading
        case loaded(MCPSetupReport)
        case failed(String)
    }

    @State private var load: Load = .loading
    @State private var isRunningSetup = false
    /// One past-tense line per client from the last real setup run.
    @State private var setupOutcome: [String] = []
    @State private var setupError: String?
    /// True when the last run changed Claude Desktop's config. It is the one
    /// client that reads its configuration only at launch.
    @State private var needsDesktopRelaunch = false
    /// The client whose snippet was just copied, so the button can confirm it.
    @State private var copiedTarget: String?
    /// The conflicting client awaiting a replace confirmation, if any.
    @State private var replacing: MCPSetupResult?

    var body: some View {
        Form {
            Section("Model Context Protocol server") {
                LabeledContent("Transport") {
                    HStack(spacing: 8) {
                        Text("stdio · launched as a child process")
                            .foregroundStyle(.secondary)
                        // The mockup shows a green "Running · pid" badge here.
                        // Nothing is running: the server exists only for as
                        // long as a client keeps it open.
                        Badge(text: "On demand", tone: .neutral)
                    }
                }
                SettingsCaption("A client starts `lumi mcp` itself when it needs it. "
                    + "Lumi runs no server in the background, opens no port, and has nothing to restart.")
            }

            Section {
                clients
            } header: {
                HStack {
                    Text("Clients")
                    Spacer()
                    Button("Refresh") { Task { await refresh() } }
                        .accessibilityLabel("Refresh MCP client status")
                        .disabled(isLoading || isRunningSetup)
                }
            }

            Section("Set up") {
                setup
            }

            if case let .loaded(report) = load {
                Section("Launch command") {
                    // The mockup pairs Command with a "Choose…" button. There
                    // is nothing to choose: the registered binary is the `lumi`
                    // inside this bundle, so the row stays and the button goes.
                    labelledCode("Command", report.command)
                    labelledCode("Full command line", report.commandLine)
                    SettingsCaption("Registered under the name \"\(report.name)\". "
                        + "The data directory is passed on the command line because a client "
                        + "starts the server with no environment.")
                }
            }

            Section("Upgrades") {
                SettingsCaption("A client holds one `lumi mcp` session open for a long time. "
                    + "That session reports schema and version skew in every tool's notice, "
                    + "and replaces its own image after an upgrade. This app does not manage it.")
            }
        }
        .formStyle(.grouped)
        .frame(minHeight: 420)
        .task { await refresh() }
        // `presenting:` rather than reading `replacing` inside the action, which
        // the Replace button clears before its work starts.
        .confirmationDialog(
            "Replace the registered entry?",
            isPresented: Binding(
                get: { replacing != nil },
                set: { if !$0 { replacing = nil } }),
            titleVisibility: .visible,
            presenting: replacing
        ) { result in
            Button("Replace", role: .destructive) {
                replacing = nil
                Task { await runSetup(replacing: result.target) }
            }
            Button("Cancel", role: .cancel) { replacing = nil }
        } message: { result in
            // Scoped to the one client by name. A blanket --force would also
            // overwrite another client's entry that somebody tuned by hand,
            // which is the thing this confirmation exists to protect.
            Text("Lumi will overwrite \(result.displayName)'s existing entry with its own. "
                + "No other client is changed.")
        }
    }

    // MARK: - Clients

    @ViewBuilder
    private var clients: some View {
        switch load {
        case .loading:
            HStack(spacing: 8) {
                ProgressView().controlSize(.small)
                Text("Checking clients…")
                    .foregroundStyle(.secondary)
            }
            // Said before the wait, not after it. The check shells out to the
            // `claude` and `codex` CLIs and verifies the binary, so several
            // seconds is the normal case rather than a hang.
            SettingsCaption("Lumi asks the Claude and Codex CLIs, which can take a few seconds.")
        case let .failed(message):
            Text(message)
                .foregroundStyle(Theme.attention)
                .fixedSize(horizontal: false, vertical: true)
        case let .loaded(report):
            if report.results.isEmpty {
                SettingsCaption("No MCP client was found on this Mac.")
            } else {
                ForEach(report.results) { result in
                    clientRow(result)
                }
            }
        }
    }

    private func clientRow(_ result: MCPSetupResult) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Text(result.displayName)
                Spacer()
                Badge(text: result.status.label, tone: Self.tone(for: result.status))
            }
            .accessibilityElement(children: .combine)

            if !result.detail.isEmpty {
                SettingsCaption(result.detail)
            }

            if result.status == .conflict, !result.current.isEmpty {
                VStack(alignment: .leading, spacing: 3) {
                    Text("Already registered as:")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    code(result.current)
                    // A button, which it deliberately was not while a `lumi`
                    // command existed to run in a terminal. Nothing about
                    // overwriting a hand-tuned entry got safer — there is just
                    // nowhere else to do it now, which is what the confirmation
                    // and the entry shown above it are for.
                    SettingsCaption("Lumi will not overwrite it unless you ask.")
                    Button("Replace…") { replacing = result }
                        .disabled(isRunningSetup || isLoading)
                        .accessibilityLabel("Replace the \(result.displayName) entry")
                }
            }

            if !result.manualHint.isEmpty {
                SettingsCaption("To do it by hand: \(result.manualHint)")
            }
            HStack {
                Button(copiedTarget == result.target ? "Copied" : "Copy client config") {
                    copy(result)
                }
                // The label follows the title. A fixed label would hide the one
                // confirmation this button gives: the switch to "Copied" is
                // visible, and nothing else announces that the copy happened.
                .accessibilityLabel(copiedTarget == result.target
                    ? "Copied the \(result.displayName) configuration snippet"
                    : "Copy the \(result.displayName) configuration snippet")
                .disabled(result.manual.isEmpty)
                Spacer()
            }
        }
        .padding(.vertical, 2)
    }

    // MARK: - Setup

    @ViewBuilder
    private var setup: some View {
        HStack {
            Button("Set up MCP") { Task { await runSetup() } }
                .buttonStyle(.borderedProminent)
                .tint(Theme.accent)
                .disabled(isRunningSetup || isLoading)
            if isRunningSetup {
                ProgressView().controlSize(.small)
            }
            Spacer()
        }
        SettingsCaption("Writes the lumi entry into every MCP client installed here. "
            + "Running it twice changes nothing.")

        if let setupError {
            Text(setupError)
                .foregroundStyle(Theme.attention)
                .fixedSize(horizontal: false, vertical: true)
        }
        ForEach(setupOutcome, id: \.self) { line in
            Text(line)
                .font(.caption)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
        }
        if needsDesktopRelaunch {
            Text("Quit and reopen Claude Desktop to load the new entry.")
                .font(.caption)
                .foregroundStyle(Theme.attention)
                .fixedSize(horizontal: false, vertical: true)
        }
    }

    // MARK: - Work

    private var isLoading: Bool {
        if case .loading = load { return true }
        return false
    }

    /// refresh re-reads the registration status.
    ///
    /// `--dry-run` is what makes this safe to run on every appearance: with it,
    /// `internal/mcpsetup` inspects each client and writes nothing.
    private func refresh() async {
        load = .loading
        do {
            let report = try await LumiCLI.json(
                MCPSetupReport.self, ["mcp", "setup", "--dry-run", "--json"])
            load = .loaded(report)
        } catch {
            load = .failed(error.localizedDescription)
        }
    }

    /// runSetup registers Lumi with every installed client, then re-reads the
    /// status so the badges describe the machine and not the run.
    ///
    /// `replacing` names the one client whose existing entry is to be
    /// overwritten, and is passed to `--client` verbatim — `internal/cli` accepts
    /// a target's own name there so this holds no second copy of that
    /// vocabulary. Scoping it is what keeps a confirmed replace from also
    /// forcing a client the user never saw.
    private func runSetup(replacing target: String? = nil) async {
        isRunningSetup = true
        setupError = nil
        setupOutcome = []
        needsDesktopRelaunch = false
        defer { isRunningSetup = false }
        do {
            var arguments = ["mcp", "setup", "--json"]
            if let target {
                arguments += ["--force", "--client", target]
            }
            // Decoded even on a non-zero exit: a conflict prints a complete
            // document and *then* fails, and that document is the answer.
            // LumiCLI.json already reads the payload before the exit status.
            let report = try await LumiCLI.json(MCPSetupReport.self, arguments)
            setupOutcome = report.results.map(Self.outcome)
            // `changed` and not `status`: a target reports `added` even when
            // the write it then attempted failed, and only `changed` is set
            // after the config actually moved. Telling someone to restart
            // Claude Desktop for a change that never landed is noise.
            needsDesktopRelaunch = report.results.contains {
                $0.target == "claude-desktop" && $0.changed
            }
            // At least one client failed. The command exits non-zero for this,
            // but LumiCLI.json decodes the payload first — deliberately, so a
            // conflict still reports per client — so the failure is read from
            // the payload rather than from the exit code.
            let failedCount = report.results.filter { !$0.succeeded }.count
            if failedCount > 0 {
                setupError = failedCount == 1
                    ? "One client could not be configured. See the list below."
                    : "\(failedCount) clients could not be configured. See the list below."
            }
        } catch {
            setupError = error.localizedDescription
        }
        await refresh()
    }

    /// copy puts the client's own snippet on the pasteboard verbatim.
    ///
    /// The snippet is never built here. `internal/mcpsetup` owns all three
    /// foreign config formats — JSON for the Claude clients, TOML for Codex —
    /// and a renderer in Swift would hand a Codex user TOML to paste into a
    /// JSON object the first time either format moved.
    private func copy(_ result: MCPSetupResult) {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(result.manual, forType: .string)
        copiedTarget = result.target
        Task {
            try? await Task.sleep(for: .seconds(2))
            if copiedTarget == result.target { copiedTarget = nil }
        }
    }

    // MARK: - Rendering

    private func labelledCode(_ title: String, _ value: String) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(title)
            code(value)
        }
    }

    /// A path or a command line, selectable so it can be copied out.
    private func code(_ value: String) -> some View {
        Text(value)
            .font(.system(.caption, design: .monospaced))
            .foregroundStyle(.secondary)
            .textSelection(.enabled)
            .fixedSize(horizontal: false, vertical: true)
    }

    /// tone maps a status onto a badge colour. Only an identical existing entry
    /// is green: everything else is either a change nobody has made yet or a
    /// reason it cannot be made.
    private static func tone(for status: MCPSetupStatus) -> Badge.Tone {
        switch status {
        case .unchanged: return .ok
        case .added, .replaced: return .warn
        case .conflict, .failed: return .bad
        // Not installed is not a problem.
        case .skipped: return .neutral
        }
    }

    /// outcome is one client's line after a real run.
    ///
    /// Deliberately not `status.label`: the badge describes the state of the
    /// machine ("Not registered"), while this describes what the run just did.
    private static func outcome(_ result: MCPSetupResult) -> String {
        // A status that claims success is checked against the error first,
        // because the status cannot carry this on its own: a target sets
        // `added` *before* it attempts the write and returns that same result
        // when the write fails. Reading the status alone would report a
        // registration for a run that registered nothing, and the refresh that
        // follows would then contradict it.
        //
        // The other statuses already say nothing was written, and they say it
        // more precisely than the error does, so they are left to the switch.
        if !result.succeeded, result.status == .added || result.status == .replaced
            || result.status == .unchanged {
            return "\(result.displayName): nothing was written — \(result.error ?? "")"
        }
        switch result.status {
        case .added: return "\(result.displayName): registered."
        case .replaced: return "\(result.displayName): entry replaced."
        case .unchanged: return "\(result.displayName): already registered."
        case .conflict:
            return "\(result.displayName): a different entry is in the way. Nothing was written."
        case .skipped: return "\(result.displayName): not installed."
        case .failed:
            return "\(result.displayName): its config could not be read. Nothing was written."
        }
    }
}
