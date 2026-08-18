import AppKit
import SwiftUI

/// Theme holds the few colours Lumi defines for itself. Everything not named
/// here is a semantic system colour on purpose, so the app follows Dark Mode,
/// increased contrast, and accessibility settings without maintaining a second
/// palette.
enum Theme {
    /// The warm amber accent: active toggles, level meters, primary buttons,
    /// and the disk-usage bar. Nothing else is tinted.
    static let accent = Color(red: 0xE7 / 255, green: 0xB2 / 255, blue: 0x4E / 255)

    /// The recording indicator. This is deliberately the system red rather than
    /// the accent — it means "capture is live", which is a status, not a brand.
    static let recording = Color(nsColor: .systemRed)
    static let live = Color(nsColor: .systemGreen)
    static let attention = Color(nsColor: .systemOrange)

    /// The window is a fixed 320pt: it holds three short states and never a
    /// list, so resizing it would only ever add empty space.
    static let windowWidth: CGFloat = 320
    /// The band the system draws the window buttons in. The custom title row
    /// matches it so the title sits on their line rather than below them.
    static let titleBarHeight: CGFloat = 38
    /// The height macOS reserves above a window's content for the title bar,
    /// asked of AppKit rather than written down: it has changed between
    /// releases, and the window's layout depends on it being right.
    static let systemTitleBarHeight: CGFloat =
        NSWindow.frameRect(forContentRect: .zero, styleMask: [.titled]).height
    static let settingsWidth: CGFloat = 660
}

/// StatusDot is the small filled circle used for every state indication, in the
/// menu and in the window.
struct StatusDot: View {
    var color: Color
    var pulsing: Bool = false
    var diameter: CGFloat = 9

    @State private var faded = false
    /// Honour Reduce Motion: the pulse carries no information the colour and
    /// label do not already carry, so it is the first thing to drop.
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    var body: some View {
        Circle()
            .fill(color)
            .frame(width: diameter, height: diameter)
            .shadow(color: color.opacity(0.7), radius: pulsing ? 4 : 0)
            .opacity(faded ? 0.55 : 1)
            .onAppear {
                guard pulsing, !reduceMotion else { return }
                withAnimation(.easeInOut(duration: 0.7).repeatForever(autoreverses: true)) {
                    faded = true
                }
            }
    }
}

/// LevelMeter draws three bars from a real measurement.
///
/// The bars are heights, never an animation loop. A meter that animates on a
/// timer claims sound was heard when nothing measured it, and the whole reason
/// the recorder emits levels at all is so this can be true. When no fresh
/// measurement exists the bars sit at their floor rather than moving.
struct LevelMeter: View {
    /// Peak and median of the most recent chunk, in dBFS, or nil when nothing
    /// recent has been measured.
    var level: AudioLevel?
    var isFresh: Bool

    private var fractions: [Double] {
        guard let level, isFresh else { return [0, 0, 0] }
        let peak = AudioLevel.normalized(level.peakDBFS)
        let median = AudioLevel.normalized(level.medianDBFS)
        // Three bars from two figures: the typical level, the midpoint, and the
        // loudest moment. Reading outward from the median is what keeps a quiet
        // chunk with one loud noise from looking like continuous speech.
        return [median, (median + peak) / 2, peak]
    }

    var body: some View {
        HStack(alignment: .center, spacing: 2.5) {
            ForEach(Array(fractions.enumerated()), id: \.offset) { _, fraction in
                Capsule()
                    .fill(Theme.accent)
                    .frame(width: 2.5, height: max(3, 13 * fraction))
            }
        }
        .frame(width: 13, height: 13, alignment: .center)
        .animation(.easeInOut(duration: 0.35), value: fractions)
        .accessibilityHidden(true)
    }
}

/// StatePill is the Required / Granted badge used on permission rows.
struct StatePill: View {
    var text: String
    var state: PermissionState

    /// Three tones, not two. "Not requested" is neither granted nor blocking —
    /// nobody has asked yet — and colouring it like a denial tells the user
    /// something is wrong when nothing is.
    private var tint: Color {
        switch state {
        case .granted: return Theme.live
        case .notDetermined: return .secondary
        case .denied, .deniedOrNotDetermined, .restricted: return Theme.recording
        }
    }

    var body: some View {
        Text(text)
            .font(.system(size: 11, weight: .semibold))
            .padding(.horizontal, 8)
            .padding(.vertical, 2)
            .foregroundStyle(tint)
            .background(Capsule().fill(tint.opacity(0.14)))
    }
}

/// Badge is StatePill's shape for everything that is not a TCC permission —
/// MCP registration, mostly. Same three tones, chosen by the caller rather than
/// derived from a permission state.
struct Badge: View {
    enum Tone { case ok, neutral, warn, bad }

    var text: String
    var tone: Tone

    private var tint: Color {
        switch tone {
        case .ok: return Theme.live
        case .neutral: return .secondary
        case .warn: return Theme.attention
        case .bad: return Theme.recording
        }
    }

    var body: some View {
        Text(text)
            .font(.system(size: 11, weight: .semibold))
            .padding(.horizontal, 8)
            .padding(.vertical, 2)
            .foregroundStyle(tint)
            .background(Capsule().fill(tint.opacity(0.14)))
    }
}

/// SettingsCaption is the grey line of explanation under a control. Every tab
/// draws them the same way, so the styling lives here rather than being
/// repeated per row.
struct SettingsCaption: View {
    var text: String

    init(_ text: String) { self.text = text }

    var body: some View {
        Text(text)
            .font(.caption)
            .foregroundStyle(.secondary)
            .fixedSize(horizontal: false, vertical: true)
    }
}

/// Format holds the two number renderings the settings tabs share, so a byte
/// count reads identically in Storage and in Danger.
enum Format {
    /// Bytes as macOS itself writes them — decimal GB, matching Finder.
    static func bytes(_ count: Int64) -> String {
        let formatter = ByteCountFormatter()
        formatter.countStyle = .file
        formatter.allowedUnits = [.useKB, .useMB, .useGB, .useTB]
        return formatter.string(fromByteCount: max(0, count))
    }

    /// A grouped item count: 48,102.
    static func count(_ value: Int) -> String {
        let formatter = NumberFormatter()
        formatter.numberStyle = .decimal
        return formatter.string(from: NSNumber(value: value)) ?? "\(value)"
    }
}
