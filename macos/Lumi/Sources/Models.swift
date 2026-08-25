import Foundation

/// RecordStatus is `lumi record status --json`, decoded.
///
/// The app reads the recorder's state through this command rather than by
/// parsing `record.json` directly. That file's format belongs to
/// `internal/cli`, and a second parser here would drift the moment either side
/// changed — invisibly, since neither test suite can see the other.
struct RecordStatus: Decodable {
    var recording: Bool
    var pid: Int?
    var startedAt: Date?
    var screen: Bool?
    var audio: Bool?
    /// Absent for a recorder the app supervises: a foreground recorder writes
    /// no log of its own, its output goes to the pipe this app holds.
    var log: String?

    static let notRecording = RecordStatus(recording: false)
}

/// PermissionState is one TCC service's status, exactly as
/// `macosnative.PermissionStatus` reports it. The raw strings are Lumi's, not
/// Apple's, and are kept verbatim so this never has to guess.
enum PermissionState: String, Decodable {
    case granted
    case denied
    case notDetermined = "not_determined"
    case deniedOrNotDetermined = "denied_or_not_determined"
    case restricted

    /// After a rebuild without a Developer ID signature, TCC no longer
    /// recognises the bundle and previously granted services read as
    /// not-determined or denied. See docs/research/2026-08-17-tcc-spike.md —
    /// when this looks wrong during development, it is usually the missing
    /// signature rather than a bug.
    var isGranted: Bool { self == .granted }

    var label: String {
        switch self {
        case .granted: return "Granted"
        case .notDetermined: return "Not requested"
        case .denied, .deniedOrNotDetermined: return "Required"
        case .restricted: return "Restricted"
        }
    }
}

/// Permissions is `lumi permissions --json`, decoded.
struct Permissions: Decodable {
    var screenRecording: PermissionState = .notDetermined
    var accessibility: PermissionState = .notDetermined
    var inputMonitoring: PermissionState = .notDetermined
    var microphone: PermissionState = .notDetermined
    var speechRecognition: PermissionState = .notDetermined

    /// One row per service, in the order `lumi permissions` prints them. The
    /// order is part of the contract the Permissions tab mirrors.
    var rows: [PermissionRow] {
        [
            PermissionRow(service: .screenRecording, state: screenRecording),
            PermissionRow(service: .accessibility, state: accessibility),
            PermissionRow(service: .inputMonitoring, state: inputMonitoring),
            PermissionRow(service: .microphone, state: microphone),
            PermissionRow(service: .speechRecognition, state: speechRecognition),
        ]
    }

    /// missingFor reports which required services are not granted, for a given
    /// capture mode.
    ///
    /// The rule is `requireRecordingPermissions` in `internal/cli/root.go` and
    /// is mirrored here rather than counted: screen capture needs Screen
    /// Recording and Accessibility; audio needs Screen Recording (the system
    /// track is a ScreenCaptureKit stream), Microphone, and Speech Recognition.
    /// Input Monitoring is never required.
    func missingFor(screen: Bool, audio: Bool) -> [PermissionRow] {
        var required: [PermissionService] = []
        if screen || audio { required.append(.screenRecording) }
        if screen { required.append(.accessibility) }
        if audio { required.append(contentsOf: [.microphone, .speechRecognition]) }
        return rows.filter { required.contains($0.service) && !$0.state.isGranted }
    }
}

enum PermissionService: String, CaseIterable, Identifiable {
    case screenRecording, accessibility, inputMonitoring, microphone, speechRecognition

    var id: String { rawValue }

    var title: String {
        switch self {
        case .screenRecording: return "Screen Recording"
        case .accessibility: return "Accessibility"
        case .inputMonitoring: return "Input Monitoring"
        case .microphone: return "Microphone"
        case .speechRecognition: return "Speech Recognition"
        }
    }

    var subtitle: String {
        switch self {
        case .screenRecording: return "ScreenCaptureKit display capture"
        case .accessibility: return "Reads on-screen text from focused app"
        case .inputMonitoring: return "Optional · sharpens capture sensitivity"
        case .microphone: return "Voice + ambient audio capture"
        case .speechRecognition: return "On-device SpeechAnalyzer"
        }
    }

    var isOptional: Bool { self == .inputMonitoring }

    /// The Privacy pane this service lives in. Deep links verified against
    /// macOS 26.5 during the TCC spike.
    var settingsURL: URL? {
        let anchor: String
        switch self {
        case .screenRecording: anchor = "Privacy_ScreenCapture"
        case .accessibility: anchor = "Privacy_Accessibility"
        case .inputMonitoring: anchor = "Privacy_ListenEvent"
        case .microphone: anchor = "Privacy_Microphone"
        case .speechRecognition: anchor = "Privacy_SpeechRecognition"
        }
        return URL(string: "x-apple.systempreferences:com.apple.preference.security?\(anchor)")
    }
}

struct PermissionRow: Identifiable {
    let service: PermissionService
    let state: PermissionState
    var id: String { service.id }
}

/// AudioLevel is one `{"event":"audio_level",...}` line from the recorder's
/// stderr, produced by `--emit-levels`.
///
/// It arrives several times a second, per track, for as long as capture runs:
/// the recorder sums sound inside the capture callback and drains it on a
/// ticker, so a meter drawn from this moves with the room. The figures are the
/// same windowed dBFS the capture pipeline uses to decide whether a chunk was
/// silent — there is no second definition of "level" anywhere in this app.
struct AudioLevel: Decodable {
    var event: String
    var source: String
    var peakDbfs: Double
    var medianDbfs: Double
    var windowMs: Int
    var durationMs: Int
    /// Whether the interval was digital silence, as `internal/capture` decides
    /// it. Read rather than computed: the floor this compares against belongs to
    /// `internal/wav`, and a second copy of the comparison here would drift.
    var silent: Bool

    /// When this app read the measurement, which is what staleness is counted
    /// from. The default is evaluated during decoding, so it is receipt time and
    /// not an accident of struct initialisation.
    ///
    /// Deliberately not the line's `captured_at`: that names the start of the
    /// sound the figures summarise, not when the app learned of it, and the two
    /// are what staleness must not confuse.
    var receivedAt = Date()

    /// `receivedAt` is not in the JSON, so the keys are named rather than
    /// synthesised. Everything the recorder sends that is absent here — most of
    /// all `captured_at` — is ignored on purpose.
    private enum CodingKeys: String, CodingKey {
        case event, source, peakDbfs, medianDbfs, windowMs, durationMs, silent
    }

    /// Digital silence, as `internal/wav.SilenceFloorDBFS` defines it. A level
    /// at the floor is a real measurement — nothing was playing — not a missing
    /// one.
    static let silenceFloorDBFS = -120.0

    /// The quietest level worth drawing as movement. Capture gain varies enough
    /// between machines that an absolute "this is speech" threshold is not
    /// portable (see internal/wav), so this is only a display range: it maps the
    /// band between a very quiet room and full scale onto the meter.
    static let meterFloorDBFS = -60.0

    /// normalized maps a dBFS reading onto 0...1 for the meter.
    static func normalized(_ dbfs: Double) -> Double {
        guard dbfs > silenceFloorDBFS else { return 0 }
        let clamped = max(meterFloorDBFS, min(0, dbfs))
        return (clamped - meterFloorDBFS) / (0 - meterFloorDBFS)
    }
}

/// MCPSetupReport is `lumi mcp setup --json`, decoded.
///
/// Read with `--dry-run` it is the read-only status query `internal/mcpsetup`
/// does not otherwise offer: every entry point there writes unless DryRun is
/// set, so the app asks what a run *would* do rather than what is registered.
/// Read without it, it is the outcome of the Set up button.
///
/// Lumi never runs or supervises an MCP server. Clients launch `lumi mcp`
/// themselves over stdio, so there is no pid here, and nothing to restart.
struct MCPSetupReport: Decodable {
    /// The name the entry is registered under, default "lumi".
    var name: String
    /// The absolute path to the binary clients are pointed at.
    var command: String
    var args: [String]
    /// command and args rendered as one line, the way the CLI shows it.
    var commandLine: String
    var dryRun: Bool
    var results: [MCPSetupResult]
}

/// MCPSetupResult is one client's row in the report.
struct MCPSetupResult: Decodable, Identifiable {
    var target: String
    var status: MCPSetupStatus
    var detail: String
    var current: String
    /// A paste-able config snippet in the client's own format — JSON for the
    /// Claude clients, TOML for Codex. It arrives on every result, whatever the
    /// status. Never build this in Swift: `internal/mcpsetup` owns what each of
    /// the three foreign config formats looks like, and a second renderer here
    /// would drift the moment one of them changed.
    var manual: String
    /// The sentence introducing `manual`, e.g. `add this under "mcpServers"`.
    /// It travels with the snippet because one hardcoded sentence would tell a
    /// Codex user to paste TOML into a JSON object.
    var manualHint: String
    var changed: Bool
    /// The failure this client reported, absent when it succeeded.
    ///
    /// The status alone cannot say whether a write landed: a target sets
    /// `added` *before* it attempts the write and returns that same result when
    /// the write fails. So a row with `status == .added` and a non-empty error
    /// registered nothing. Read `failure` before believing `status`.
    var error: String?

    var id: String { target }

    /// succeeded reports whether this client ended up holding what Lumi asked
    /// for. Under `--dry-run` it reports whether the same run would have
    /// succeeded: a conflict fails in both modes, deliberately, so a preview
    /// stays a faithful preview.
    var succeeded: Bool { error == nil || error?.isEmpty == true }

    /// The client's display name. The raw values are `internal/mcpsetup`'s
    /// target names; an unrecognised one is shown as-is rather than dropped, so
    /// a newer lumi that knows a fourth client still lists it.
    var displayName: String {
        switch target {
        case "claude-code": return "Claude Code"
        case "claude-desktop": return "Claude Desktop"
        case "codex": return "Codex CLI"
        default: return target
        }
    }
}

/// MCPSetupStatus mirrors `mcpsetup.Status`.
enum MCPSetupStatus: String, Decodable {
    /// No entry existed and one was written — or, under --dry-run, would be.
    case added
    /// An identical entry already exists. This is what "registered" looks like.
    case unchanged
    case replaced
    /// A different entry is in the way and Lumi will not overwrite it.
    case conflict
    /// The client is not installed on this machine.
    case skipped
    /// The client is installed but could not be inspected, so Lumi does not
    /// know what it holds. Deliberately distinct from conflict: nothing is in
    /// the way, so "replace it" is advice that cannot work.
    case failed

    /// isRegistered reports whether the client will launch Lumi today.
    /// Only an identical existing entry means that; everything else is either a
    /// change waiting to be made or a reason it cannot be.
    var isRegistered: Bool { self == .unchanged }

    /// The badge text, in the same three tones the permission pills use.
    var label: String {
        switch self {
        case .unchanged: return "Registered"
        case .added, .replaced: return "Not registered"
        case .conflict: return "Conflict"
        case .skipped: return "Not installed"
        case .failed: return "Unreadable"
        }
    }
}

/// PruneResult is `lumi prune --json`, decoded — `retention.Result`.
///
/// The app never deletes a row or a file itself. Row-before-file ordering is
/// `internal/retention`'s rule and stays there; this is only how the result of
/// asking is read back.
struct PruneResult: Decodable {
    var events: Int64
    var bytes: Int64
    var missingFiles: Int
    /// Files removed by the `--all` sweep that no row referenced. Their bytes
    /// are already counted in `bytes`.
    var orphanFiles: Int
}

/// CompressResult is `lumi compress --json`, decoded — `compress.Result`.
///
/// The app runs the command and reads the counters back; what compression means
/// — which codecs, what quality, when a re-encode is rejected — is
/// `internal/compress`'s and stays there. The two passes are summed for display
/// because the user pressed one button, not two.
struct CompressResult: Decodable {
    /// Pass is `compress.PassResult`. Every field is emitted unconditionally by
    /// the CLI, so none is optional here.
    struct Pass: Decodable {
        var files: Int64
        var bytesBefore: Int64
        var bytesAfter: Int64
        var alreadyDone: Int64
        var skipped: Int64
        var missingFiles: Int64
        var encodeFailed: Int64
        var verifyFailed: Int64
        var raced: Int64
        var conflicted: Int64
        var flushFailed: Int64

        /// Every file the run declined to replace. They are counted together
        /// because the answer is the same for all of them: the original is
        /// still on disk and still indexed. The per-reason breakdown is the
        /// CLI's own output, which stays the fuller report.
        var untouched: Int64 {
            skipped + missingFiles + encodeFailed + verifyFailed + raced + conflicted + flushFailed
        }
    }

    /// Reconciled is what an interrupted earlier run left behind.
    struct Reconciled: Decodable {
        var removed: Int64
        var recovered: Int64
    }

    /// Vacuum is the database rebuild. Only `status` is always present.
    struct Vacuum: Decodable {
        var status: String
        var detail: String?
        var bytesBefore: Int64?
        var bytesAfter: Int64?
    }

    var screens: Pass
    var audio: Pass
    var reconciled: Reconciled
    var vacuum: Vacuum

    var files: Int64 { screens.files + audio.files }
    var bytesBefore: Int64 { screens.bytesBefore + audio.bytesBefore }
    var bytesAfter: Int64 { screens.bytesAfter + audio.bytesAfter }
    var alreadyDone: Int64 { screens.alreadyDone + audio.alreadyDone }
    var untouched: Int64 { screens.untouched + audio.untouched }
}
