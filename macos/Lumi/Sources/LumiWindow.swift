import AppKit
import SwiftUI

/// LumiWindow is the small floating window behind "Open Lumi".
///
/// It has exactly three mutually exclusive states and no navigation. Recording
/// is the normal one: Lumi is always-on, so there is no start/stop ceremony,
/// no timer, no session name, and no "processing" state to wait through.
struct LumiWindow: View {
    @Environment(RecorderController.self) private var recorder
    @Environment(\.openSettings) private var openSettings

    var body: some View {
        VStack(spacing: 0) {
            titleBar
            Divider()
            content
                .frame(maxWidth: .infinity)
            Divider()
            footer
        }
        .frame(width: Theme.windowWidth)
        .background(.regularMaterial)
        // The content extends under the title bar so the title row can sit in
        // the same band as the window buttons. Without this the system reserves
        // the title bar's height above the content, and the title lands well
        // below the buttons it is supposed to line up with.
        .ignoresSafeArea(.container, edges: .top)
    }

    // MARK: - Chrome

    private var titleBar: some View {
        ZStack {
            Text("lumi")
                .font(.system(size: 13, weight: .semibold))
                .foregroundStyle(.secondary)
            HStack {
                Spacer()
                Button {
                    openSettings()
                } label: {
                    Image(systemName: "gearshape")
                        .font(.system(size: 12, weight: .medium))
                        .frame(width: 24, height: 24)
                }
                .buttonStyle(.accessoryBar)
                .accessibilityLabel("Open Settings")
                .help("Settings")
            }
        }
        .padding(.horizontal, 14)
        // Height rather than padding, so the row's centre — and with it the
        // title and the gear — falls on the same line as the close, minimise,
        // and zoom buttons the system draws over it.
        .frame(height: Theme.titleBarHeight)
    }

    @ViewBuilder
    private var content: some View {
        switch recorder.state {
        case .recording: recordingState
        case .idle: idleState
        case .needsPermissions: permissionsState
        }
    }

    private var footer: some View {
        HStack {
            Text("On-device · nothing leaves your Mac")
                .font(.system(size: 11))
                .foregroundStyle(.secondary)
            Spacer()
            // Plain, and the same weight and colour as the line beside it. The
            // link style rendered it accent-blue, which read as a hyperlink and
            // gave the one destructive action in the window more emphasis than
            // anything else on screen.
            Button("Quit") { LumiApp.confirmQuit() }
                .buttonStyle(.plain)
                .font(.system(size: 11))
                .foregroundStyle(.secondary)
                .pointerStyle(.link)
                .accessibilityLabel("Quit Lumi")
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 9)
    }

    // MARK: - Recording

    private var recordingState: some View {
        VStack(spacing: 16) {
            HStack(spacing: 7) {
                StatusDot(color: Theme.recording, pulsing: true)
                Text("Recording")
                    .font(.system(size: 14, weight: .semibold))
                    .foregroundStyle(Theme.recording)
                    .textCase(.uppercase)
                    .kerning(0.3)
            }
            .accessibilityElement(children: .ignore)
            .accessibilityLabel("Recording")

            VStack(spacing: 7) {
                if Preferences.shared.captureScreen {
                    StatusRow(
                        title: "Screen capture",
                        detail: recorder.displayCount == 1 ? "1 display" : "\(recorder.displayCount) displays",
                        healthy: true)
                }
                if Preferences.shared.captureAudio {
                    StatusRow(
                        title: "Microphone",
                        level: recorder.level(for: "microphone"),
                        isFresh: recorder.hasFreshLevel(for: "microphone"),
                        healthy: true)
                    StatusRow(
                        title: "System audio",
                        level: recorder.level(for: "system"),
                        isFresh: recorder.hasFreshLevel(for: "system"),
                        healthy: true)
                }
            }

            if let error = recorder.lastError {
                Text(error)
                    .font(.system(size: 11))
                    .foregroundStyle(Theme.attention)
                    .multilineTextAlignment(.center)
                    .fixedSize(horizontal: false, vertical: true)
            }

            Button(recorder.isStopping ? "Stopping…" : "Stop recording") {
                Task { await recorder.stop() }
            }
            .buttonStyle(StopButtonStyle())
            .disabled(recorder.isStopping)
        }
        .padding(.horizontal, 24)
        .padding(.top, 20)
        .padding(.bottom, 24)
    }

    // MARK: - Idle

    private var idleState: some View {
        VStack(spacing: 18) {
            Button {
                Task { await recorder.start() }
            } label: {
                ZStack {
                    Circle()
                        .fill(Color.primary.opacity(0.08))
                        .frame(width: 104, height: 104)
                    Circle()
                        .fill(LinearGradient(
                            colors: [Color(nsColor: .systemRed).opacity(0.92), Theme.recording],
                            startPoint: .top, endPoint: .bottom))
                        .frame(width: 66, height: 66)
                }
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Start recording")

            VStack(spacing: 3) {
                Text("Ready to record")
                    .font(.system(size: 17, weight: .semibold))
                Text("Continuously capture screen & audio into your searchable memory.")
                    .font(.system(size: 13))
                    .foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)
                    .frame(maxWidth: 220)
                    .fixedSize(horizontal: false, vertical: true)
            }

            if let error = recorder.lastError {
                Text(error)
                    .font(.system(size: 11))
                    .foregroundStyle(Theme.attention)
                    .multilineTextAlignment(.center)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
        .padding(.horizontal, 24)
        .padding(.top, 22)
        .padding(.bottom, 26)
    }

    // MARK: - Permissions

    private var permissionsState: some View {
        VStack(spacing: 14) {
            Image(systemName: "exclamationmark.triangle.fill")
                .font(.system(size: 26))
                .foregroundStyle(Theme.accent)

            VStack(spacing: 3) {
                Text("Permissions needed")
                    .font(.system(size: 17, weight: .semibold))
                Text("Lumi needs system access before it can capture.")
                    .font(.system(size: 13))
                    .foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)
                    .fixedSize(horizontal: false, vertical: true)
            }

            // Every required service, granted or not — the list is what the
            // rule in `requireRecordingPermissions` says is required for the
            // current capture mode, never a fixed count.
            VStack(spacing: 7) {
                ForEach(requiredRows) { row in
                    permissionRow(row)
                }
            }

            // Said plainly, because the alternative is the user waiting for a
            // dialog that is never coming. Screen Recording and Accessibility
            // do not re-prompt once macOS holds a decision for this build, and
            // an unsigned rebuild is a new build every time. The measurement is
            // docs/research/2026-08-17-tcc-spike.md, Result 6.
            if hasSettingsOnlyService {
                Text("Screen Recording and Accessibility never ask twice. "
                     + "Switch them on in System Settings, then come back.")
                    .font(.system(size: 11))
                    .foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)
                    .fixedSize(horizontal: false, vertical: true)
            }

            // Explained before requested, never the other way round: the app
            // must not raise a system prompt the user has had no chance to
            // understand.
            //
            // This runs the request flows, which is what Microphone and Speech
            // Recognition need — they answer with a dialog. It no longer opens
            // a pane afterwards. It used to open the *first* missing service's
            // pane, which meant that with both Screen Recording and
            // Accessibility missing, the second one had no route at all: the
            // user granted the first, came back, and the same button sent them
            // to the same place. Each row carries its own route now.
            Button("Request access") {
                Task { await recorder.requestPermissions() }
            }
            .buttonStyle(.borderedProminent)
            .tint(Theme.accent)
            .help("Ask macOS for the permissions that answer with a dialog")
        }
        .padding(.horizontal, 24)
        .padding(.top, 20)
        .padding(.bottom, 24)
    }

    /// permissionRow is one service. An ungranted one is a button that opens
    /// its own pane in System Settings.
    ///
    /// Per service, not one button for the set. Two of these services can only
    /// ever be fixed in System Settings, and they live in two different panes;
    /// a single destination leaves whichever one is not first unreachable.
    @ViewBuilder
    private func permissionRow(_ row: PermissionRow) -> some View {
        let body = HStack {
            Text(row.service.title)
                .font(.system(size: 13))
            Spacer()
            StatePill(text: row.state.label, state: row.state)
            if !row.state.isGranted, row.service.settingsURL != nil {
                Image(systemName: "chevron.right")
                    .font(.system(size: 10, weight: .semibold))
                    .foregroundStyle(.secondary)
            }
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 9)
        .background(RoundedRectangle(cornerRadius: 10).fill(Color.primary.opacity(0.05)))

        if !row.state.isGranted, let url = row.service.settingsURL {
            Button {
                NSWorkspace.shared.open(url)
            } label: {
                body
            }
            .buttonStyle(.plain)
            .accessibilityLabel("\(row.service.title), \(row.state.label). Open it in System Settings")
            .help("Open \(row.service.title) in System Settings")
        } else {
            body.accessibilityElement(children: .combine)
        }
    }

    /// hasSettingsOnlyService reports whether anything missing is one macOS
    /// will not re-prompt for. Screen Recording and Accessibility are the two;
    /// both are reached only through System Settings once a decision exists.
    private var hasSettingsOnlyService: Bool {
        recorder.missingPermissions.contains {
            $0.service == .screenRecording || $0.service == .accessibility
        }
    }

    // There is deliberately no "can this still prompt?" test. Screen Recording
    // and Accessibility report `denied_or_not_determined`, and that conflation
    // is on purpose: `lumi_permissions_json` says splitting the two needs Full
    // Disk Access or a call that prompts as a side effect. So "never asked" and
    // "already refused" are indistinguishable here, and disabling the button on
    // that guess would suppress a prompt that would have worked. Running the
    // flows again costs nothing when there is nothing to ask.

    /// requiredRows is every service the current capture mode needs, with the
    /// ungranted ones first so the thing to fix is at the top.
    private var requiredRows: [PermissionRow] {
        let missing = recorder.missingPermissions
        let missingIDs = Set(missing.map(\.id))
        let granted = recorder.permissions.rows.filter {
            !$0.service.isOptional && !missingIDs.contains($0.id) && $0.state.isGranted
        }
        return missing + granted
    }
}

/// StatusRow is one source's line in the recording card.
private struct StatusRow: View {
    var title: String
    var detail: String?
    var level: AudioLevel?
    var isFresh: Bool = false
    var healthy: Bool

    init(title: String, detail: String, healthy: Bool) {
        self.title = title
        self.detail = detail
        self.healthy = healthy
    }

    init(title: String, level: AudioLevel?, isFresh: Bool, healthy: Bool) {
        self.title = title
        self.level = level
        self.isFresh = isFresh
        self.healthy = healthy
    }

    var body: some View {
        HStack(spacing: 10) {
            Text(title)
                .font(.system(size: 13))
            Spacer()
            if let detail {
                Text(detail)
                    .font(.system(size: 12))
                    .foregroundStyle(.secondary)
            } else {
                LevelMeter(level: level, isFresh: isFresh)
            }
            StatusDot(color: healthy ? Theme.live : Theme.attention, diameter: 7)
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 9)
        .background(RoundedRectangle(cornerRadius: 10).fill(Color.primary.opacity(0.05)))
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(accessibilityLabel)
    }

    private var accessibilityLabel: String {
        if let detail { return "\(title), \(detail)" }
        guard let level, isFresh else { return "\(title), no recent measurement" }
        return "\(title), peak \(Int(level.peakDBFS)) decibels full scale"
    }
}

/// StopButtonStyle keeps stopping low-emphasis. Recording is the normal state
/// and stopping is the exception, so this is text-sized and only turns red on
/// hover — never a primary call to action.
private struct StopButtonStyle: ButtonStyle {
    @State private var hovering = false

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(.system(size: 12))
            .foregroundStyle(hovering ? Theme.recording : Color.secondary)
            .padding(.horizontal, 10)
            .padding(.vertical, 4)
            .contentShape(Rectangle())
            .onHover { hovering = $0 }
            .opacity(configuration.isPressed ? 0.6 : 1)
    }
}
