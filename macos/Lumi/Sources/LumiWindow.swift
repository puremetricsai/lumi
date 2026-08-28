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
            content
                .frame(maxWidth: .infinity)
            footer
        }
        .frame(width: Theme.windowWidth)
        .background(.regularMaterial)
        // The content extends under the title bar so the title row can sit in
        // the same band as the window buttons. Without this the system reserves
        // the title bar's height above the content, and the title lands well
        // below the buttons it is supposed to line up with.
        .ignoresSafeArea(.container, edges: .top)
        // …and the window still sizes itself as if that band were above the
        // content, which left the band's height as dead space under the footer.
        // The negative padding gives it back. It is the measured band rather
        // than a constant: the height changes between macOS releases.
        .padding(.bottom, -Theme.systemTitleBarHeight)
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
                }
                .buttonStyle(.accessoryBar)
                .accessibilityLabel("Open Settings")
                .help("Settings")
            }
        }
        // No padding on either side. The gear used to sit 14pt in from an
        // already generously padded .accessoryBar button, which left its glyph
        // 32pt from the right edge while the close button sits 10pt from the
        // left. Flush, the glyph lands ~13pt in — the same band the traffic
        // lights occupy — and the title centres on the window rather than on a
        // padded row. The explicit 24pt square around the glyph went with the
        // padding: .accessoryBar draws its own hit area, and the square only
        // pushed the gear further from the edge.
        //
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

    /// The footer carries the local-first guarantee and nothing else.
    ///
    /// Quit used to sit here. It lives in the menu bar menu now, which is where
    /// a menu bar app is quit from, and it is the only place: two routes out of
    /// the app would be two places to keep the graceful-stop confirmation
    /// right.
    private var footer: some View {
        HStack {
            Text("On-device · nothing leaves your Mac")
                .font(.system(size: 11))
                .foregroundStyle(.secondary)
            Spacer()
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
                    AudioStatusRow(
                        title: "Microphone",
                        level: recorder.level(for: "microphone"),
                        silenceIsFailure: true)
                    AudioStatusRow(title: "System audio", level: recorder.level(for: "system"))
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
            //
            // The second sentence is Result 5, and it is the one that gets
            // people stuck: TCC stores its own requirement for the grant,
            // pinning the *leaf certificate* that earned it, so a build signed
            // with a different certificate fails that requirement while the
            // switch stays visibly on. "Switch them on" is then advice the user
            // has already followed. The addendum to Result 5 has the tccd trace.
            //
            // It says "try", and names no specific repair, deliberately. What
            // is measured is that rewriting the row recovers it; which gesture
            // rewrites it is not — Result 5's own recovery was a `tccutil
            // reset` before re-enabling, and removing the row outright works
            // too. Nothing here can detect the case either: telling a stale
            // grant from a plain refusal needs Full Disk Access to read TCC.db,
            // which is why this is a caption and not a branch.
            if hasSettingsOnlyService {
                Text("Screen Recording and Accessibility never ask twice. "
                     + "Switch them on in System Settings, then come back. "
                     + "If a switch already looks on, try turning it off and on "
                     + "again — an entry left by an earlier build can read as "
                     + "enabled and still be denied.")
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
            Badge(text: row.state.label, tone: row.state.tone)
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

/// AudioStatusRow is one audio track's line in the recording card.
///
/// A missing level is drawn as a missing level. `RecorderController` prunes a
/// measurement the recorder has stopped refreshing, so nil here means one of
/// two things — capture has only just started, or the track has stopped
/// reporting because the microphone is denied, absent, or gone — and neither is
/// "sound was heard". Bars at their floor beside a green dot said the opposite
/// of both, and a dead microphone was indistinguishable from a quiet room.
///
/// Levels are live, so this clears within a second of capture starting. A row
/// still reading "No signal yet" after that is a real answer about that track.
///
/// The recorder's own `silent` means opposite things on the two tracks, which is
/// `silenceIsFailure`. Nothing playing is what a system track sounds like most
/// of the time; a live microphone in a silent room still carries its own noise,
/// so a microphone reporting silence is one nothing is reaching — a denied or
/// stale Microphone grant delivers empty buffers rather than an error, and this
/// row is the only place that shows. Judged one interval at a time, because a
/// live microphone does not produce one.
private struct AudioStatusRow: View {
    var title: String
    var level: AudioLevel?
    var silenceIsFailure = false

    private var isDigitallySilent: Bool { silenceIsFailure && level?.silent == true }

    var body: some View {
        StatusRow(
            title: title,
            detail: level == nil ? "No signal yet"
                : isDigitallySilent ? "Silent — check Microphone access" : nil,
            level: level,
            healthy: level != nil && !isDigitallySilent)
    }
}

/// StatusRow is one source's line in the recording card.
private struct StatusRow: View {
    var title: String
    var detail: String? = nil
    var level: AudioLevel? = nil
    var healthy: Bool

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
                LevelMeter(level: level)
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
        guard let level else { return "\(title), no recent measurement" }
        return "\(title), peak \(Int(level.peakDbfs)) decibels full scale"
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
