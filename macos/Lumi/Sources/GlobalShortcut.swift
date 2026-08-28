import AppKit
import Carbon.HIToolbox
import Observation
import SwiftUI

/// RecordingShortcut is a key combination the user assigned to start/stop.
///
/// `modifiers` is an `NSEvent.ModifierFlags` raw value rather than the flags
/// themselves, so `Preferences` can store and return one without importing
/// AppKit. `label` is what `charactersIgnoringModifiers` gave at record time,
/// kept so displaying the shortcut needs neither a key-code table nor
/// `UCKeyTranslate` — the key code is what Carbon registers, and the label is
/// only ever shown.
struct RecordingShortcut: Equatable {
    var keyCode: UInt16
    var modifiers: UInt
    var label: String

    var flags: NSEvent.ModifierFlags { NSEvent.ModifierFlags(rawValue: modifiers) }

    /// The combination as macOS writes it, in the conventional ⌃⌥⇧⌘ order.
    var display: String {
        var glyphs = ""
        if flags.contains(.control) { glyphs += "⌃" }
        if flags.contains(.option) { glyphs += "⌥" }
        if flags.contains(.shift) { glyphs += "⇧" }
        if flags.contains(.command) { glyphs += "⌘" }
        return glyphs + label
    }
}

/// GlobalShortcut owns the one system-wide hotkey and the Settings control that
/// assigns it.
///
/// Carbon `RegisterEventHotKey`, not `NSEvent.addGlobalMonitorForEvents`. A
/// global monitor needs Input Monitoring, which `Models.swift` deliberately
/// models as *optional* and `PermissionsSettings` renders as a neutral "Off"
/// when denied — a hotkey built on it would quietly promote that row into a
/// requirement, and would be dead under the Hardened Runtime unless
/// `Lumi.entitlements` named it. Carbon needs no grant and no entitlement.
///
/// `action` is filled in by `AppDelegate`, exactly like `LumiApp.hide` and for
/// the same reason: `NSApp.delegate as? AppDelegate` is nil and silent under
/// `@NSApplicationDelegateAdaptor`, so there is no reference to reach the
/// delegate through. Settings writes the preference and calls `reload()`; it
/// never learns what the shortcut does.
@Observable
@MainActor
final class GlobalShortcut {
    static let shared = GlobalShortcut()

    /// What the hotkey runs. Set once, at launch.
    @ObservationIgnored var action: (() -> Void)?

    /// Whether the last `reload()` failed to arm the hotkey. Shown in Settings
    /// rather than swallowed: the alternative is a shortcut that silently does
    /// nothing and no way to find out why.
    ///
    /// This is *not* collision detection, and must not be described as it.
    /// `RegisterEventHotKey` is asked for `kEventHotKeyNoOptions`, which is
    /// non-exclusive: a combination another app already holds registers
    /// successfully here and then fires in both apps. Measured — a second
    /// process claiming a combination Lumi held was given `noErr`. Nothing
    /// Carbon offers reports what another app owns, which is the reason the
    /// shortcut ships unassigned rather than with a default nobody vetted.
    private(set) var registrationFailed = false

    @ObservationIgnored private var hotKey: EventHotKeyRef?
    @ObservationIgnored private var handler: EventHandlerRef?

    private init() {}

    /// reload registers whatever `Preferences` now holds, replacing any
    /// previous registration. Unassigned means "registered nothing", which is
    /// the shipped default — Lumi claims no combination until asked to.
    func reload() {
        suspend()
        registrationFailed = false
        guard let shortcut = Preferences.shared.recordingShortcut else { return }

        // Registering with no handler installed would let the system swallow
        // the combination while nothing ever ran.
        guard installHandler() else {
            registrationFailed = true
            return
        }
        var ref: EventHotKeyRef?
        let status = RegisterEventHotKey(
            UInt32(shortcut.keyCode),
            Self.carbonModifiers(shortcut.flags),
            EventHotKeyID(signature: Self.signature, id: 1),
            GetApplicationEventTarget(),
            0,
            &ref)
        if status == noErr {
            hotKey = ref
        } else {
            registrationFailed = true
        }
    }

    /// suspend unregisters without forgetting the preference.
    ///
    /// A registered hotkey is consumed by the system before it ever becomes an
    /// `NSEvent`, so the recorder below must suspend before it starts
    /// listening: otherwise pressing the *current* combination to confirm or
    /// replace it would toggle recording instead of being captured.
    func suspend() {
        if let hotKey { UnregisterEventHotKey(hotKey) }
        hotKey = nil
    }

    private func fire() { action?() }

    /// The Carbon event handler, installed once and left in place. Its callback
    /// is a C function pointer and can capture nothing, so it reaches the
    /// singleton directly.
    private func installHandler() -> Bool {
        if handler != nil { return true }
        var spec = EventTypeSpec(
            eventClass: OSType(kEventClassKeyboard), eventKind: UInt32(kEventHotKeyPressed))
        let status = InstallEventHandler(
            GetApplicationEventTarget(),
            { _, _, _ -> OSStatus in
                DispatchQueue.main.async { MainActor.assumeIsolated { GlobalShortcut.shared.fire() } }
                return noErr
            },
            1, &spec, nil, &handler)
        // `handler` stays nil on failure, so the next reload tries again.
        return status == noErr
    }

    private static let signature = OSType(0x6C75_6D69)  // 'lumi'

    private static func carbonModifiers(_ flags: NSEvent.ModifierFlags) -> UInt32 {
        var mask: UInt32 = 0
        if flags.contains(.command) { mask |= UInt32(cmdKey) }
        if flags.contains(.option) { mask |= UInt32(optionKey) }
        if flags.contains(.control) { mask |= UInt32(controlKey) }
        if flags.contains(.shift) { mask |= UInt32(shiftKey) }
        return mask
    }
}

/// ShortcutRecorder is the Settings row: the current combination, a button that
/// listens for a new one, and the line of explanation under it.
///
/// The listener is `addLocalMonitorForEvents`, not a global one — it needs no
/// permission, and it only has to see keys typed into a Settings window that is
/// already key.
struct ShortcutRecorder: View {
    // Held in the view's own state, written through to Preferences. The
    // preference is a computed property over UserDefaults, so it reports no
    // change to @Observable and a title read straight off it would not redraw
    // after a commit or a clear — the same reason StorageRelocationNotice holds
    // a stored property.
    @State private var shortcut = Preferences.shared.recordingShortcut
    @State private var capturing = false
    @State private var monitor: Any?
    @State private var needsModifier = false
    @State private var globalShortcut = GlobalShortcut.shared

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            // One button, not a button and a Clear beside it. LabeledContent
            // hands its own label to everything it wraps and wins over
            // .accessibilityLabel, so two buttons here both announce
            // "Start / stop recording" and VoiceOver cannot tell them apart.
            // Delete already clears while capturing, and the caption says so.
            LabeledContent("Start / stop recording") {
                Button(buttonTitle) { capturing ? cancel() : begin() }
                    .frame(minWidth: 130)
                    // The label is LabeledContent's; the value is what the
                    // button actually shows, and is the only way a VoiceOver
                    // user hears which combination is assigned.
                    .accessibilityValue(shortcut?.display ?? "not set")
            }
            SettingsCaption(caption)
        }
        .onDisappear(perform: cancel)
    }

    private var buttonTitle: String {
        if capturing { return "Press keys…" }
        return shortcut?.display ?? "Record Shortcut"
    }

    private var caption: String {
        // Before `capturing`, not after: a rejected key leaves capture running,
        // so an order that answers `capturing` first makes this message
        // unreachable and a bare keypress look like nothing happened.
        if needsModifier {
            return "That needs ⌘, ⌃, or ⌥ too — a key on its own would fire while you were typing."
        }
        if capturing {
            return "Hold ⌘, ⌃, or ⌥ and press a key. Escape cancels, Delete clears."
        }
        // Deliberately not "another app already uses it": Carbon registers
        // non-exclusively and cannot tell us that.
        if globalShortcut.registrationFailed {
            return "Lumi could not register that combination. Try a different one."
        }
        if shortcut == nil {
            return "Not set. Lumi claims no combination until you choose one."
        }
        return "Starts and stops recording while any other app is frontmost."
    }

    private func begin() {
        // Each monitor must be removed exactly once, and a second begin() would
        // overwrite the first token and strand it swallowing keys for the rest
        // of the process. The button cannot do this today; the guard is what
        // keeps that true.
        guard monitor == nil else { return }
        needsModifier = false
        capturing = true
        // Suspend first: the live hotkey would otherwise swallow its own
        // combination before this monitor could see it.
        globalShortcut.suspend()
        monitor = NSEvent.addLocalMonitorForEvents(matching: .keyDown) { event in
            handle(event)
            return nil  // swallowed, so nothing types into the window behind it
        }
    }

    private func handle(_ event: NSEvent) {
        switch Int(event.keyCode) {
        case kVK_Escape:
            cancel()
            return
        case kVK_Delete, kVK_ForwardDelete:
            commit(nil)
            return
        default:
            break
        }

        let flags = event.modifierFlags.intersection(.deviceIndependentFlagsMask)
            .intersection([.command, .control, .option, .shift])
        // Shift alone is not enough: a global hotkey on ⇧A would fire in the
        // middle of anyone's sentence.
        guard !flags.intersection([.command, .control, .option]).isEmpty else {
            needsModifier = true
            return
        }
        guard let typed = event.charactersIgnoringModifiers, typed.count == 1,
              let scalar = typed.unicodeScalars.first, !CharacterSet.controlCharacters.contains(scalar)
        else {
            needsModifier = true
            return
        }

        commit(RecordingShortcut(
            keyCode: event.keyCode, modifiers: flags.rawValue, label: typed.uppercased()))
    }

    private func commit(_ new: RecordingShortcut?) {
        needsModifier = false
        shortcut = new
        Preferences.shared.recordingShortcut = new
        endCapture()
    }

    private func cancel() {
        needsModifier = false
        endCapture()
    }

    /// Every exit — commit, cancel, Settings closing — removes the monitor and
    /// puts the hotkey back. `reload()` rather than a saved ref, so it always
    /// registers whatever the preference now holds.
    private func endCapture() {
        if let monitor { NSEvent.removeMonitor(monitor) }
        monitor = nil
        capturing = false
        globalShortcut.reload()
    }
}
