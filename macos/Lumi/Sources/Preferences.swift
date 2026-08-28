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
@Observable
final class Preferences {
    static let shared = Preferences()

    /// Keys are namespaced so a future preference cannot collide with a
    /// framework default.
    private enum Key {
        static let captureScreen = "lumi.captureScreen"
        static let captureAudio = "lumi.captureAudio"
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
    }

    var captureScreen: Bool {
        get { defaults.bool(forKey: Key.captureScreen) }
        set { defaults.set(newValue, forKey: Key.captureScreen) }
    }

    /// Whether the app asks GitHub, once a day, whether a newer release exists.
    ///
    /// App policy rather than a capture flag, so it is deliberately absent from
    /// `recorderArguments()`. What the request contains and what "newer" means
    /// are both Go's; this only decides whether it is made at all.
    var checkForUpdates: Bool {
        get { defaults.bool(forKey: Key.checkForUpdates) }
        set { defaults.set(newValue, forKey: Key.checkForUpdates) }
    }

    var captureAudio: Bool {
        get { defaults.bool(forKey: Key.captureAudio) }
        set { defaults.set(newValue, forKey: Key.captureAudio) }
    }

    /// Seconds, matching `--interval` (default 2s).
    var intervalSeconds: Double {
        get { defaults.double(forKey: Key.intervalSeconds) }
        set { defaults.set(newValue, forKey: Key.intervalSeconds) }
    }

    /// Seconds, matching `--audio-chunk` (default 30s). It sets transcription
    /// granularity only — the level meters are live and do not depend on it.
    var audioChunkSeconds: Double {
        get { defaults.double(forKey: Key.audioChunkSeconds) }
        set { defaults.set(newValue, forKey: Key.audioChunkSeconds) }
    }

    /// Matching `--speech-locale` (default en-US).
    var speechLocale: String {
        get { defaults.string(forKey: Key.speechLocale) ?? "en-US" }
        set { defaults.set(newValue, forKey: Key.speechLocale) }
    }

    /// The shared data directory.
    ///
    /// Unset means "whatever the CLI would choose", which is resolved the same
    /// way `internal/config.DefaultPaths` does — `LUMI_HOME`, else
    /// `~/Library/Application Support/Lumi` — so app and CLI land on the same
    /// store without the app having to store a path at all.
    var dataDirectory: String {
        get {
            if let stored = defaults.string(forKey: Key.dataDirectory), !stored.isEmpty {
                return stored
            }
            return Preferences.defaultDataDirectory
        }
        set {
            defaults.set(newValue, forKey: Key.dataDirectory)
        }
    }

    /// hasCustomDataDirectory reports whether the user chose a location, as
    /// opposed to inheriting the default. Shown in Settings so "this is where it
    /// happens to be" and "this is where you put it" stay distinguishable.
    var hasCustomDataDirectory: Bool {
        guard let stored = defaults.string(forKey: Key.dataDirectory) else { return false }
        return !stored.isEmpty && stored != Preferences.defaultDataDirectory
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
    ///
    /// `shortcutLabel` is the presence test: a key code of 0 is a real key (A),
    /// so it cannot stand for "unset".
    var recordingShortcut: RecordingShortcut? {
        get {
            guard let label = defaults.string(forKey: Key.shortcutLabel), !label.isEmpty else {
                return nil
            }
            // `exactly:`, not a plain conversion: these are three independent
            // values anybody can edit, and a negative or oversized one traps
            // and takes the app down at launch. A shortcut that does not
            // round-trip is no shortcut.
            guard let keyCode = UInt16(exactly: defaults.integer(forKey: Key.shortcutKeyCode)),
                  let modifiers = UInt(exactly: defaults.integer(forKey: Key.shortcutModifiers))
            else { return nil }
            return RecordingShortcut(keyCode: keyCode, modifiers: modifiers, label: label)
        }
        set {
            guard let shortcut = newValue else {
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
        return args
    }
}
