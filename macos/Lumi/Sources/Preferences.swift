import Foundation
import Observation

/// Preferences is the app's own settings, and only the app's.
///
/// Lumi has no configuration file and this does not introduce one.
/// `internal/config` resolves *paths*; every capture setting is a CLI flag whose
/// default lives in `recordFlags.bind`. So these values are stored in the app's
/// UserDefaults and translated into flags when the recorder is spawned — that
/// translation is the whole mechanism. The CLI keeps its own defaults and is
/// unaffected by anything set here.
///
/// The defaults below must stay identical to `recordFlags.bind`, or the same
/// machine would capture differently depending on which interface started it.
///
/// Every property is a *stored* property seeded from UserDefaults in `init` and
/// written back in `didSet`. That is what `@Observable` instruments — the macro
/// moves the `didSet` onto its own backing storage and wraps the accessors, so
/// persistence and observation both hold. A computed property over UserDefaults
/// registers no dependency and a view reading one is never invalidated.
@Observable
final class Preferences {
    static let shared = Preferences()

    /// Keys are namespaced so a future preference cannot collide with a
    /// framework default.
    private enum Key {
        static let captureScreen = "lumi.captureScreen"
        static let captureAudio = "lumi.captureAudio"
        static let selectedDisplayIDs = "lumi.selectedDisplayIDs"
        static let intervalSeconds = "lumi.intervalSeconds"
        static let audioChunkSeconds = "lumi.audioChunkSeconds"
        static let speechLocale = "lumi.speechLocale"
        static let dataDirectory = "lumi.dataDirectory"
        static let checkForUpdates = "lumi.checkForUpdates"
        static let shortcutKeyCode = "lumi.shortcutKeyCode"
        static let shortcutModifiers = "lumi.shortcutModifiers"
        static let shortcutLabel = "lumi.shortcutLabel"
    }

    private let defaults: UserDefaults

    init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
        defaults.register(defaults: [
            Key.captureScreen: true,
            Key.captureAudio: true,
            Key.intervalSeconds: 2.0,
            Key.audioChunkSeconds: 30.0,
            Key.speechLocale: "en-US",
            Key.checkForUpdates: true,
        ])
        // Seeded from the `defaults` parameter, not `self.defaults`: phase-1
        // initialisation forbids reading `self` before every stored property
        // has a value. `didSet` does not fire here, so seeding writes nothing
        // back.
        captureScreen = defaults.bool(forKey: Key.captureScreen)
        captureAudio = defaults.bool(forKey: Key.captureAudio)
        checkForUpdates = defaults.bool(forKey: Key.checkForUpdates)
        intervalSeconds = defaults.double(forKey: Key.intervalSeconds)
        audioChunkSeconds = defaults.double(forKey: Key.audioChunkSeconds)
        speechLocale = defaults.string(forKey: Key.speechLocale) ?? "en-US"
        selectedDisplayIDs = (defaults.array(forKey: Key.selectedDisplayIDs) as? [Int] ?? [])
            .compactMap { UInt32(exactly: $0) }
        dataDirectory = {
            if let stored = defaults.string(forKey: Key.dataDirectory), !stored.isEmpty {
                return stored
            }
            return Preferences.defaultDataDirectory
        }()
        recordingShortcut = Preferences.storedShortcut(in: defaults)
    }

    var captureScreen: Bool {
        didSet { defaults.set(captureScreen, forKey: Key.captureScreen) }
    }

    var captureAudio: Bool {
        didSet { defaults.set(captureAudio, forKey: Key.captureAudio) }
    }

    /// Whether the app asks GitHub, once a day, whether a newer release exists.
    ///
    /// App policy rather than a capture flag, so it is deliberately absent from
    /// `recorderArguments()`. What the request contains and what "newer" means
    /// are both Go's; this only decides whether it is made at all.
    var checkForUpdates: Bool {
        didSet { defaults.set(checkForUpdates, forKey: Key.checkForUpdates) }
    }

    /// The displays to record, or empty for every connected one.
    ///
    /// No default is registered, the same way `dataDirectory` registers none:
    /// empty is a real answer here and it is exactly `recordFlags.bind`'s own
    /// default. Which displays are connected is not consulted — a monitor that
    /// is unplugged stays selected, so plugging it back in restores the choice
    /// rather than silently dropping it.
    ///
    /// Seeded through `UInt32(exactly:)` and filtered, like `recordingShortcut`:
    /// these are values anybody can edit, and a negative one would reach the
    /// recorder as `--displays -5`, which the binary refuses — the whole
    /// recorder would fail to start over a stray default.
    var selectedDisplayIDs: [UInt32] {
        didSet {
            if selectedDisplayIDs.isEmpty {
                defaults.removeObject(forKey: Key.selectedDisplayIDs)
            } else {
                defaults.set(selectedDisplayIDs.map(Int.init), forKey: Key.selectedDisplayIDs)
            }
        }
    }

    /// Seconds, matching `--interval` (default 2s).
    var intervalSeconds: Double {
        didSet { defaults.set(intervalSeconds, forKey: Key.intervalSeconds) }
    }

    /// Seconds, matching `--audio-chunk` (default 30s). It sets transcription
    /// granularity only — the level meters are live and do not depend on it.
    var audioChunkSeconds: Double {
        didSet { defaults.set(audioChunkSeconds, forKey: Key.audioChunkSeconds) }
    }

    /// Matching `--speech-locale` (default en-US).
    var speechLocale: String {
        didSet { defaults.set(speechLocale, forKey: Key.speechLocale) }
    }

    /// The shared data directory.
    ///
    /// Unset means "whatever the CLI would choose", which is resolved the same
    /// way `internal/config.DefaultPaths` does — `LUMI_HOME`, else
    /// `~/Library/Application Support/Lumi` — so app and CLI land on the same
    /// store without the app having to store a path at all.
    var dataDirectory: String {
        didSet { defaults.set(dataDirectory, forKey: Key.dataDirectory) }
    }

    /// hasCustomDataDirectory reports whether the user chose a location, as
    /// opposed to inheriting the default. Shown in Settings so "this is where it
    /// happens to be" and "this is where you put it" stay distinguishable.
    var hasCustomDataDirectory: Bool {
        !dataDirectory.isEmpty && dataDirectory != Preferences.defaultDataDirectory
    }

    static var defaultDataDirectory: String {
        if let home = ProcessInfo.processInfo.environment["LUMI_HOME"], !home.isEmpty {
            return home
        }
        return FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent("Library/Application Support/Lumi").path
    }

    /// The global start/stop key combination, or nil when none is assigned.
    ///
    /// No default is registered for these keys, the same way `dataDirectory`
    /// registers none: unset is a real answer here. Shipping a combination
    /// would claim one that another app may already hold, and Carbon would
    /// simply refuse the registration with nothing on screen to say so.
    ///
    /// App policy rather than a capture flag, so it is deliberately absent from
    /// `recorderArguments()` — the binary knows nothing about this, and the
    /// Settings control must not call `RecordingSettings.restart()`. The
    /// precedent is `checkForUpdates` above.
    var recordingShortcut: RecordingShortcut? {
        didSet {
            guard let shortcut = recordingShortcut else {
                defaults.removeObject(forKey: Key.shortcutKeyCode)
                defaults.removeObject(forKey: Key.shortcutModifiers)
                defaults.removeObject(forKey: Key.shortcutLabel)
                return
            }
            defaults.set(Int(shortcut.keyCode), forKey: Key.shortcutKeyCode)
            defaults.set(Int(shortcut.modifiers), forKey: Key.shortcutModifiers)
            defaults.set(shortcut.label, forKey: Key.shortcutLabel)
        }
    }

    /// `shortcutLabel` is the presence test: a key code of 0 is a real key (A),
    /// so it cannot stand for "unset". `exactly:`, not a plain conversion:
    /// these are three independent values anybody can edit, and a negative or
    /// oversized one traps and takes the app down at launch. A shortcut that
    /// does not round-trip is no shortcut.
    private static func storedShortcut(in defaults: UserDefaults) -> RecordingShortcut? {
        guard let label = defaults.string(forKey: Key.shortcutLabel), !label.isEmpty,
              let keyCode = UInt16(exactly: defaults.integer(forKey: Key.shortcutKeyCode)),
              let modifiers = UInt(exactly: defaults.integer(forKey: Key.shortcutModifiers))
        else { return nil }
        return RecordingShortcut(keyCode: keyCode, modifiers: modifiers, label: label)
    }

    /// recorderArguments translates these preferences into the argv for the
    /// recorder the app supervises.
    ///
    /// Three flags here are not user preferences and are not settable:
    /// `--foreground` keeps the recorder a directly-held child so TCC attributes
    /// its prompts to this bundle rather than to launchd;
    /// `--register-state` publishes it in `record.json` so `lumi record status`,
    /// `record stop`, `compress`, and `transcript backfill` can all see the
    /// recorder the app owns; and `--emit-levels` is what feeds the meters.
    func recorderArguments() -> [String] {
        var args = [
            "record", "start", "--foreground", "--register-state", "--emit-levels",
            "--data-dir", dataDirectory,
            "--interval", "\(Int(intervalSeconds))s",
            "--audio-chunk", "\(Int(audioChunkSeconds))s",
            "--speech-locale", speechLocale,
        ]
        if !captureScreen { args.append("--no-screen") }
        if !captureAudio { args.append("--no-audio") }
        // Omitted when empty rather than passed as an empty list: the binary's
        // own default is "every connected display", and spelling that here
        // would be a second copy of it.
        if !selectedDisplayIDs.isEmpty {
            args += ["--displays", selectedDisplayIDs.map(String.init).joined(separator: ",")]
        }
        return args
    }
}
