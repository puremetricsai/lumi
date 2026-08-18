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

    enum CodingKeys: String, CodingKey {
        case recording, pid, screen, audio, log
        case startedAt = "started_at"
    }

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

extension PermissionState {
    /// An unknown value is treated as not-determined rather than as a decode
    /// failure: a newer `lumi` reporting a state this build has never heard of
    /// must not take the whole permissions screen down with it.
    init(from decoder: Decoder) throws {
        let raw = try decoder.singleValueContainer().decode(String.self)
        self = PermissionState(rawValue: raw) ?? .notDetermined
    }
}

/// Permissions is `lumi permissions --json`, decoded.
struct Permissions: Decodable {
    var screenRecording: PermissionState = .notDetermined
    var accessibility: PermissionState = .notDetermined
    var inputMonitoring: PermissionState = .notDetermined
    var microphone: PermissionState = .notDetermined
    var speechRecognition: PermissionState = .notDetermined

    enum CodingKeys: String, CodingKey {
        case screenRecording = "screen_recording"
        case accessibility
        case inputMonitoring = "input_monitoring"
        case microphone
        case speechRecognition = "speech_recognition"
    }

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
/// It arrives once per finished chunk, because that is when audio reaches Go at
/// all. A meter drawn from it refreshes at `--audio-chunk`, and the figures are
/// the same windowed dBFS the capture pipeline uses to decide whether a chunk
/// was silent — there is no second definition of "level" anywhere in this app.
struct AudioLevel: Decodable {
    var event: String
    var source: String
    var capturedAt: Date
    var peakDBFS: Double
    var medianDBFS: Double
    var windowMS: Int
    var durationMS: Int

    enum CodingKeys: String, CodingKey {
        case event, source
        case capturedAt = "captured_at"
        case peakDBFS = "peak_dbfs"
        case medianDBFS = "median_dbfs"
        case windowMS = "window_ms"
        case durationMS = "duration_ms"
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
