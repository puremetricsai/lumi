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

    /// The toolbar capsule. The window is the capsule and nothing else — no
    /// title bar, no fixed width — so it sizes itself to whichever controls the
    /// current state draws.
    static let barHeight: CGFloat = 44
    static let barItemHeight: CGFloat = 30
    static let barSpacing: CGFloat = 8
    static let settingsWidth: CGFloat = 660
}

/// ToolbarButtonStyle is every button in the toolbar capsule.
///
/// One style, not one per control: the hover tint is the only thing that
/// differs. Red on hover is reserved for the controls that end something —
/// record's own stop, and quit — so hovering a destructive control says so
/// before it is clicked. The gear takes the default and merely brightens.
struct ToolbarButtonStyle: ButtonStyle {
    var tint: Color = .secondary
    var hoverTint: Color = .primary
    var size: CGFloat = Theme.barItemHeight

    @State private var hovering = false

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .foregroundStyle(hovering ? hoverTint : tint)
            .frame(width: size, height: size)
            .background(Circle().fill(Color.primary.opacity(hovering ? 0.12 : 0)))
            .contentShape(Circle())
            .onHover { hovering = $0 }
            .opacity(configuration.isPressed ? 0.6 : 1)
            .animation(.easeOut(duration: 0.12), value: hovering)
    }
}

/// ToolbarPill is one status group in the recording toolbar: an icon, a
/// measurement, and a health dot in their own softer capsule.
struct ToolbarPill<Content: View>: View {
    @ViewBuilder var content: Content

    var body: some View {
        HStack(spacing: 6) {
            content
        }
        .padding(.horizontal, 10)
        .frame(height: Theme.barItemHeight)
        .background(Capsule().fill(Color.primary.opacity(0.07)))
    }
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
/// the recorder emits levels at all is so this can be true. With no measurement
/// the bars sit at their floor rather than moving — and the row draws "No signal
/// yet" instead, because floor bars alone read as "measured, and quiet".
struct LevelMeter: View {
    /// Peak and median of the most recent chunk, in dBFS, or nil when nothing
    /// recent has been measured. `RecorderController` prunes a stale
    /// measurement, so a non-nil level here is a recent one by construction.
    var level: AudioLevel?

    private var fractions: [Double] {
        guard let level else { return [0, 0, 0] }
        let peak = AudioLevel.normalized(level.peakDbfs)
        let median = AudioLevel.normalized(level.medianDbfs)
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
                    // 3 + 10·f, not max(3, 13·f): the latter pinned every bar at
                    // the 3pt floor until f passed 0.23, so nothing under about
                    // -46 dBFS moved at all. That silently cost the median bar,
                    // which is room tone in a normal room and therefore always
                    // below that — leaving the one figure that separates speech
                    // from a door slam flat whatever was heard.
                    .frame(width: 2.5, height: 3 + 10 * fraction)
            }
        }
        .frame(width: 13, height: 13, alignment: .center)
        // Short enough to keep up with a live reading several times a
        // second. At 0.35s the easing was still travelling when the next
        // measurement replaced it, which reads as lag rather than as sound.
        .animation(.easeOut(duration: 0.12), value: fractions)
        .accessibilityHidden(true)
    }
}

/// Badge is the small capsule used for every status: TCC permissions, MCP
/// registration, whatever else. A permission derives its tone through
/// `PermissionState.tone`; everything else names one.
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

extension PermissionState {
    /// Three tones, not two. "Not requested" is neither granted nor blocking —
    /// nobody has asked yet — and colouring it like a denial tells the user
    /// something is wrong when nothing is.
    var tone: Badge.Tone {
        switch self {
        case .granted: return .ok
        case .notDetermined: return .neutral
        case .denied, .deniedOrNotDetermined, .restricted: return .bad
        }
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
