# macos

`Lumi.app`: the product. A SwiftUI menu-bar shell around the embedded `lumi` binary, which is the whole
of the implementation and is never installed or documented as a command of its own. `Lumi/Sources/` holds the app delegate and
menu bar item (`LumiApp.swift`), the binary runner (`LumiCLI.swift`), the recorder supervisor
(`RecorderController.swift`), the decoded JSON (`Models.swift`), the Lumi window (`LumiWindow.swift`),
and one file per Settings tab — the six run Recording (in `SettingsWindow.swift`), `StorageSettings`,
`MCPSettings`, `DangerSettings`, `PermissionsSettings`, `AboutSettings`, an order that is deliberate and
explained where it is written. `build-app.sh` builds the bundle; `task app` runs it, `task app:install`
places it at `~/Applications/Lumi.app`, and `../scripts/restart-lumi-app.sh` is the development loop.

The app is a supervisor and nothing else — root `CLAUDE.md` states that rule; this file is how it is kept.

- **Every capability comes from running the embedded binary with `--json` and decoding it.** Swift never
  opens the database, reads media, calls a capture framework, or writes a file the binary owns. A rule that
  has a Go home is read from there, never restated here: `record status --json` rather than parsing
  `record.json` (`internal/cli/CLAUDE.md` owns that format), `mcp setup --dry-run --json` rather than
  inspecting a client's config, `Result.manual_hint` rather than rendering a client's JSON or TOML
  (`internal/mcpsetup/CLAUDE.md`), a result's own `target` handed back to `--client` rather than a name
  table. Every invocation passes the app's `--data-dir`, because a child inherits no shell environment.
- **`LumiCLI.decoder` maps keys with `.convertFromSnakeCase`, so `Models.swift` carries no key tables.**
  A model that needs a `CodingKeys` enum anyway must spell its cases in the *converted* names — a case
  written `= "manual_hint"` matches nothing once the strategy has already produced `manualHint`, and the
  field decodes as absent with no error anywhere. Go's field name is the contract; spell it in camelCase
  and it lands.
- **The app-owned recorder is a `record start --foreground --register-state` child, held for its whole
  life.** Not a detached start: launchd would become the TCC responsible process instead of the bundle, and
  the grants belong to the bundle. `--register-state` is what makes it visible to `record status`, `record
  stop`, `compress`, `transcript backfill`, and the duplicate-start refusal in a terminal — and what makes
  the app refuse to start a second one over a terminal's. `--emit-levels` feeds the meters, several JSON
  lines a second per track on stderr, measured live inside the capture callback
  (`internal/capture/CLAUDE.md`).
- **The display pill counts displays being *recorded*, which only the recorder knows.** The count comes from
  the `screen_capture` line the same `--emit-levels` stream carries, and a missing or stale report is drawn
  as missing — the levels rule, for the same reason. `NSScreen.screens.count` is not the fallback: it
  answers how many displays are *connected*, which stopped being the same question when display selection
  arrived, and a number from it beside a green dot is the display-pill spelling of floor bars and a
  hardcoded `healthy: true`. Staleness comes off the report's own `interval_ms`, not `Preferences`, so a
  recorder started from a terminal with its own `--interval` still ages correctly.
- **Which displays exist, and what is on them, come from `lumi displays --json`.** Swift calls no capture
  framework: the thumbnails are the binary's, captured through the same path the recorder uses. `NSScreen`
  supplies the display's *name* and *resolution* and nothing else — labels macOS holds and Lumi has no
  opinion about, matched on the CoreGraphics display ID Go already reported. Nothing structural may depend
  on that match: a display macOS does not present as a screen still lists, still previews, and is still
  selectable by ID. The command runs on tab-appear, on a screen-parameter change, and from Refresh — never
  on a timer, because every call is a real screen capture.
- **`loadDisplays` repeats *both* of the picker's branches as guards, and the failed list is emptied rather
  than left standing.** `.task(id:)` runs its body on first appearance whatever the id evaluates to — the id
  decides only when it runs *again* — so the guards cannot be left to the id: without `captureScreen`,
  opening Settings screenshots every display for a picker that renders `EmptyView()`, after the user said
  not to capture the screen; without the Screen Recording grant, opening Settings is what raises a TCC
  prompt. And the list is read as the *connected* set by the selection canonicalisation and by
  `isOnlySelected`, so a stale one is not cosmetic: unchecking a display that is already gone slips past the
  last-display guard and stores a selection naming nothing connected, which the recorder answers by
  recording every display — the opposite of what was asked, and it outlives the app. That is why a failed
  load clears the rows and why an unplugged monitor reloads the list.
- **Every `Preferences` property is *stored*, seeded from `UserDefaults` in `init` and written back in
  `didSet`. A computed property over `UserDefaults` is the bug.** The `@Observable` macro instruments
  stored properties and nothing else, so a view reading a computed `captureScreen` registers no dependency
  and is never invalidated — the stored value is correct, the flags the recorder is started with are
  correct, and only the screen never changes. Toggling Capture screen off and back on left the display
  picker gone until the tab was reopened, which is what surfaced it. The macro relocates `didSet` onto its
  own backing storage and wraps the accessors, so persistence and observation both hold; the shape needs no
  convention a new preference could forget, which is why it replaced a hand-kept revision counter.
  Seeding reads the `defaults` *parameter*, not `self.defaults` — phase-1 initialisation forbids `self`
  before every stored property has a value, and `didSet` does not fire during `init`, so seeding writes
  nothing back.
- **A level's staleness is counted from when the app received it, and a stale one is pruned rather than
  tested at draw time.** Freshness turns on wall-clock, which `@Observable` cannot track: nothing else in
  the recording card changes while capture is healthy, so a draw-time check never re-ran and the meters held
  their last height for as long as the window stayed open — a claim about sound nobody measured.
  `RecorderController.dropStaleLevels` prunes on the existing poll, which makes presence in `levels` *be*
  freshness, so no reader asks twice. Levels arrive continuously now, silence included, so a track going
  stale means capture stopped rather than the room going quiet.
- **A missing level is drawn as missing — an amber dot and "No signal yet" in the tooltip, never floor
  bars and a green one.** A track that wrote no file is never an event (`internal/macosnative`), so a denied or absent
  microphone produces no level at all, and bars at their floor beside a hardcoded `healthy: true` rendered
  that identically to a quiet room. Levels are live, so this clears within a second of capture starting: a
  pill still reading "No signal yet" after that is a real answer about that track.
- **Opening Settings goes through `LumiApp.openSettings`, and only a SwiftUI view can fill it.** There is
  no AppKit route to a SwiftUI `Settings` scene. The private `showSettingsWindow:` selector that used to
  serve one is gone in macOS 26 — AppKit raises NSInvalidArgumentException — so the menu bar item and
  `lumi://settings` crashed the app. `LumiWindow` hands its `\.openSettings` action over on appear and
  `AppDelegate` calls it; the window appears at launch, which is what makes it there before the menu is.
  Nothing may reach the delegate with `NSApp.delegate as? AppDelegate`: `@NSApplicationDelegateAdaptor`
  leaves `NSApp.delegate` holding SwiftUI's own forwarding delegate, so that cast is nil and silent.
- **Stopping is SIGTERM then wait (`stopTimeout`, 20s), never SIGKILL** — in-flight media is still being
  written and indexed. This is also the app-quit path: `AppDelegate` returns `.terminateLater` while a
  child is held, so ⌘Q asks first and then shuts down gracefully.
- **The menu bar is the only place Lumi is quit from, and the toolbar's x only hides.** An x in a window
  is read as "quit" by anyone who has met a menu bar app, so the button says otherwise in its tooltip;
  capture keeps running with every window closed, which is the point of the app. Nothing outside
  `AppDelegate.quit()` may end the process — it owns the "Stop recording and quit Lumi?" confirmation and
  the graceful stop above, and `NSApp.terminate` skips the question and shortens the wait.
- **The window is the toolbar capsule and has no chrome at all.** `.windowStyle(.plain)`, then
  `WindowChrome` in `LumiWindow` clears the background, floats the window above other apps, and turns
  `isMovableByWindowBackground` *off* — the Lumi mark's `WindowDragGesture` is the only thing that moves
  it, so dragging from a button presses the button. The toolbar has no room for prose: a source's
  sentence is its `.help` and its accessibility label, never a caption.
- **Hiding the window is `AppDelegate.hideWindow()`, reached from three places and implemented once.**
  The x button and Esc go through `LumiApp.hide`, parked exactly like `openSettings` but set by
  `AppDelegate` in `applicationDidFinishLaunching`, since unlike an `OpenSettingsAction` it needs no
  SwiftUI environment to read; the menu bar item calls it directly. Not `\.dismiss`: the menu item is
  titled on `lumiWindow?.isVisible` — `isVisible` alone, because a floating window is routinely visible
  without being key — so a close nobody told the status item about leaves it offering "Hide Lumi" for a
  window that is gone. **Esc is a local `NSEvent` monitor in `WindowChrome`, not `onExitCommand`**, which
  never fired: SwiftUI routes a cancel action through whatever holds focus and this window holds none.
  Both need the window to be *key*, and `.plain` alone leaves a borderless window that cannot become one
  — which is what `WindowChrome`'s `.titled` insert is for. Esc while another app is frontmost is not a
  bug and needs a global event tap nobody wants.
- **The toolbar row is `.fixedSize(horizontal: true, vertical: false)`, and the window is sized from
  that.** Without it the window proposes a width the row must fit into, an `HStack` shrinks the only
  flexible thing it holds, and `Text` measures zero — "REC" and the display count vanished while every
  icon and dot beside them looked right, so the bug reads as "text does not render" rather than as a
  layout one. Any label added to the bar depends on this modifier staying.
- **The global start/stop shortcut is Carbon `RegisterEventHotKey`, never a global `NSEvent` monitor.**
  A global monitor needs Input Monitoring, which `Models.swift` models as *optional* and
  `PermissionsSettings` draws as a neutral "Off" when denied — building the shortcut on it would silently
  promote that row into a requirement, and the Hardened Runtime would deny it in a release unless
  `Lumi.entitlements` named it. Carbon costs no grant and no entitlement. Two consequences that are
  invisible until they bite: a registered hotkey is consumed before it is ever an `NSEvent`, so
  `ShortcutRecorder` must `suspend()` before it listens or pressing the current combination re-records
  nothing and toggles capture instead; and **`RegisterEventHotKey` is non-exclusive**, so a combination
  another app already holds registers with `noErr` here and then fires in *both* apps — measured, a second
  process was given `noErr` for a combination Lumi held. `registrationFailed` therefore means "could not
  arm it", never "somebody else has it", and no wording anywhere may promise collision detection Carbon
  cannot do. That is the reason the shortcut ships **unassigned**: a default nobody vetted would silently
  double up on whatever already owns those keys. `GlobalShortcut.action` is parked by `AppDelegate` exactly like `LumiApp.hide`, for the same
  `NSApp.delegate as? AppDelegate` reason. It is app policy, not a capture flag: absent from
  `recorderArguments()`, and its setter must not `restart()`.
- **`RecorderController.canToggleRecording` is the one gate on whether start/stop would do anything**, read
  by the menu bar item's `isEnabled` and by the shortcut alike. `AppDelegate.toggleRecording` reads the
  recorder at fire time, which the menu deliberately does not — that rule is about a stale *label* on an
  open dropdown, and a shortcut has none.
- **Never restart the recorder when displays change.** The Go recorder rediscovers displays every interval.
- **Anything that acts on the recorder reads `isSupervisingRecorder`, never `state == .recording`.** A
  permission revoked mid-capture moves the UI to `.needsPermissions` while the child keeps running and keeps
  writing. A settings change gated on the UI state saves the setting and leaves the live recorder on the old
  flags; the menu bar's Start/Stop item titled on it offers "Start Recording" beside a running child. That
  menu sets `autoenablesItems = false` — AppKit re-enables any item whose target answers its action — and
  `RecorderController` fires `statusDidChange` unconditionally when the child exits, because the exit is a
  visible change even when `state` does not move.
- **Children get `FileHandle.nullDevice` for stdin, so no prompt can ever be answered.** Anything
  interactive must be pre-answered — `prune --all --yes`, gated by the app's own typed confirmation sheet.
- **Every *development* rebuild destroys the app's TCC grants.** A local bundle is ad-hoc signed
  (`codesign -s -`), so its code identity changes each build and Screen Recording and Accessibility land on
  `denied`, which does not re-prompt. Measured in `docs/research/2026-08-17-tcc-spike.md`; batch UI changes
  into single builds, and reach for `tccutil reset <service> com.puremetricsai.lumi` when a System Settings
  row reads enabled while the app reads denied. Not a bug to fix in code, and not what a released bundle
  does: `build-app.sh` signs with `CODESIGN_IDENTITY`, ad-hoc only by default, and a release passes a real
  identity whose designated requirement is the certificate rather than the hash — so those grants survive
  an upgrade. `docs/release.md` owns that half. A release also gets the Hardened Runtime, which
  denies any service `macos/Lumi/Resources/Lumi.entitlements` does not name — so a permission can
  work in development and be dead in a release. Read that file before any Go or Swift when one is.
- **The executable is `Contents/MacOS/LumiApp`, beside the embedded `Contents/MacOS/lumi`.** macOS
  filesystems are case-insensitive, so naming it `Lumi` makes it the same file as `lumi` and the binary
  silently overwrites the app. `build-app.sh` compares inodes and fails loudly if that returns.
- **`lumi app` hands off over the `lumi://` URL scheme, delivered to a resolved bundle path.**
  LaunchServices drops `--args` to an already-running app, which is a menu bar app's normal state, and
  AppleScript would cost an Automation prompt. The Go side of this is `internal/cli/CLAUDE.md`.
- **Nothing in the app may tell the user to run a command.** There is no `lumi` on anyone's `PATH` to run:
  the app is the whole product, and an instruction to open a terminal is an instruction to do something
  impossible. `MCPSettings` is where this was bought — a conflicting MCP entry used to name
  `lumi mcp setup --force` as a command and now offers **Replace…** behind a `.confirmationDialog`.
  The reason it was *not* a button before still stands and is why the confirmation exists: overwriting an
  entry somebody tuned by hand must be asked for, and the entry it would replace is shown above the button.
  **That replace is scoped to one client** by passing the result's own `target` to `--client`; a blanket
  `--force` would also overwrite entries for clients the user never looked at. `internal/cli` accepts a
  target's name there precisely so Swift holds no second copy of that vocabulary.
- **The app holds no install URL, no version comparison, and no knowledge of how an upgrade is
  performed.** It runs `lumi update --json` and `lumi update --apply`; `internal/cli` owns the
  `latest` pointer, what "newer" means, the two refusals, and the fact that the installer is
  `install.sh`. The one half that *is* the app's is whether to ask at all — `Preferences.checkForUpdates`,
  app policy rather than a capture flag, so it stays out of `recorderArguments()`. A second checker
  anywhere would be two requests free to disagree, which is why `UpdateChecker` is owned by
  `AppDelegate` and injected into Settings the same way `RecorderController` is.
- **Taking an update asks once, stops the recorder, and only then starts the installer.** That
  ordering is load-bearing, not tidiness. `install.sh` quits a running Lumi by Apple event, and
  `applicationShouldTerminate` answers that by stopping and then replying `true` whatever the stop
  returned — so an installer started first can race a slow shutdown and carry the app out from over a
  child that is still writing. Nothing can race a shutdown that already finished. **`stopFailed` is
  checked before the installer is started**, where the only cost of stopping is the update itself:
  `stop()` returns normally after its 20s timeout with the child still alive, and a refusal from the
  binary at that point restarts the recorder rather than leaving capture silently off.
  `confirmAndInstallUpdate` deliberately does not call `quit()`, which would stack its own "Stop
  recording and quit Lumi?" on top of the question already answered, and the app quits itself rather
  than letting `install.sh` send the event, which would cost a needless Automation prompt.
- **A failed automatic check is silent; a failed Check Now is shown.** An offline laptop must not
  leave an error in a tab nobody opened, and a button somebody pressed owes them an answer. The status
  badge reads "Not checked yet" rather than "Up to date" before the first answer returns: claiming a
  build is current before anything was asked is the one wrong answer here nobody would notice.
- **The build stays on the default Swift language mode.** `-swift-version 6` fails today:
  `Preferences.shared` is a `static let` on a non-Sendable `@Observable` class, and `LumiCLI` captures
  non-Sendable values across its pipe reads.

There is no Swift test target. Changes are verified by driving the built app, which needs the grants
re-established after the rebuild; the Go side of each seam is covered in `internal/cli`.
