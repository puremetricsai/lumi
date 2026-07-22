import Foundation
import AVFoundation
import Speech

/// Trivial liveness probe proving the Swift static archive links into the Go
/// binary and is callable across the cgo boundary. Returns a heap string the Go
/// side frees with C.free (see nativeString in native.go).
@_cdecl("lumi_speech_ping")
public func lumi_speech_ping() -> UnsafeMutablePointer<CChar>? {
    return strdup("pong")
}

/// Sets *error_message (when non-nil) to a freshly allocated copy of message and
/// returns NULL. Every failure path routes through here so a nil return always
/// carries a diagnostic — an empty string is never a stand-in for a transcript.
private func fail(_ errorMessage: UnsafeMutablePointer<UnsafeMutablePointer<CChar>?>?, _ message: String) -> UnsafeMutablePointer<CChar>? {
    if let errorMessage = errorMessage {
        errorMessage.pointee = strdup(message)
    }
    return nil
}

/// Normalizes a possibly-nil C locale string to a non-empty identifier,
/// defaulting to en-US so every entry point resolves the same way.
private func resolveLocaleID(_ locale: UnsafePointer<CChar>?) -> String {
    let requested = locale.map { String(cString: $0) } ?? ""
    return requested.isEmpty ? "en-US" : requested
}

/// Transcribes a WAV file with on-device SpeechAnalyzer. Returns a heap C string
/// (freed by Go via C.free) on success. On ANY failure returns NULL and sets
/// *error_message — never an empty string masquerading as a transcript.
///
/// timeoutSeconds bounds the blocking wait; <= 0 selects a generous default.
/// On timeout the analysis Task is cancelled (SpeechAnalyzer work is async and
/// cancellation-cooperative) and NULL is returned with a diagnostic, so the
/// caller's OS thread can never wedge indefinitely.
@_cdecl("lumi_transcribe_audio_string")
public func lumi_transcribe_audio_string(
    _ audioPath: UnsafePointer<CChar>?,
    _ locale: UnsafePointer<CChar>?,
    _ timeoutSeconds: Double,
    _ errorMessage: UnsafeMutablePointer<UnsafeMutablePointer<CChar>?>?
) -> UnsafeMutablePointer<CChar>? {
    guard let audioPath = audioPath else {
        return fail(errorMessage, "transcribe: nil audio path")
    }
    let path = String(cString: audioPath)
    let localeID = resolveLocaleID(locale)
    let timeout = timeoutSeconds > 0 ? timeoutSeconds : 120

    let semaphore = DispatchSemaphore(value: 0)
    var resultText: String?
    var errorText: String?

    let task = Task {
        defer { semaphore.signal() }
        do {
            let loc = Locale(identifier: localeID)
            let transcriber = SpeechTranscriber(
                locale: loc,
                transcriptionOptions: [],
                reportingOptions: [],
                attributeOptions: []
            )

            // Ensure the locale's on-device assets are installed. The request is
            // nil when everything needed is already present, so a download only
            // happens on first use of a locale. lumi_speech_ensure_assets should
            // have done this before recording; this is a bounded fallback.
            if let request = try await AssetInventory.assetInstallationRequest(supporting: [transcriber]) {
                try await request.downloadAndInstall()
            }

            let analyzer = SpeechAnalyzer(modules: [transcriber])
            let url = URL(fileURLWithPath: path)
            let audioFile = try AVAudioFile(forReading: url)

            // Drain the results stream concurrently with analysis. The stream
            // terminates when the analyzer finishes, ending this loop.
            let collector = Task { () -> String in
                var text = ""
                for try await result in transcriber.results {
                    text += String(result.text.characters)
                }
                return text
            }

            _ = try await analyzer.analyzeSequence(from: audioFile)
            try await analyzer.finalizeAndFinishThroughEndOfInput()
            resultText = try await collector.value
        } catch {
            errorText = "transcribe \(localeID): \(error.localizedDescription)"
        }
    }

    if semaphore.wait(timeout: .now() + timeout) == .timedOut {
        task.cancel()
        return fail(errorMessage, "transcribe \(localeID): timed out after \(Int(timeout))s")
    }

    if let errorText = errorText {
        return fail(errorMessage, errorText)
    }
    guard let resultText = resultText else {
        return fail(errorMessage, "transcribe: no result produced")
    }
    return strdup(resultText)
}

/// Downloads and installs the on-device SpeechAnalyzer assets for a locale so a
/// first transcription mid-recording never triggers an unbounded network
/// download. Returns a heap "ok" string on success; on failure or timeout
/// returns NULL with a diagnostic. timeoutSeconds <= 0 selects a generous
/// default; on timeout the install Task is cancelled.
@_cdecl("lumi_speech_ensure_assets")
public func lumi_speech_ensure_assets(
    _ locale: UnsafePointer<CChar>?,
    _ timeoutSeconds: Double,
    _ errorMessage: UnsafeMutablePointer<UnsafeMutablePointer<CChar>?>?
) -> UnsafeMutablePointer<CChar>? {
    let localeID = resolveLocaleID(locale)
    let timeout = timeoutSeconds > 0 ? timeoutSeconds : 600

    let semaphore = DispatchSemaphore(value: 0)
    var errorText: String?

    let task = Task {
        defer { semaphore.signal() }
        do {
            let transcriber = SpeechTranscriber(
                locale: Locale(identifier: localeID),
                transcriptionOptions: [],
                reportingOptions: [],
                attributeOptions: []
            )
            if let request = try await AssetInventory.assetInstallationRequest(supporting: [transcriber]) {
                try await request.downloadAndInstall()
            }
        } catch {
            errorText = "ensure speech assets \(localeID): \(error.localizedDescription)"
        }
    }

    if semaphore.wait(timeout: .now() + timeout) == .timedOut {
        task.cancel()
        return fail(errorMessage, "ensure speech assets \(localeID): timed out after \(Int(timeout))s")
    }
    if let errorText = errorText {
        return fail(errorMessage, errorText)
    }
    return strdup("ok")
}

/// Reports whether the on-device SpeechAnalyzer assets for a locale are already
/// installed. Returns 1 when installed, 0 when not, -1 on error/timeout. The
/// authoritative signal is SpeechTranscriber.installedLocales: it lists the
/// locales whose assets are actually present on device. We deliberately do NOT
/// use `AssetInventory.assetInstallationRequest(supporting:) == nil` — that
/// request comes back non-nil even for an already-installed locale (it wants to
/// re-reserve/allocate), so nil-ness reports a permanent false "missing" and
/// made `lumi doctor` fail despite working transcription. This is a local
/// query, so the wait is bounded short.
@_cdecl("lumi_speech_assets_installed")
public func lumi_speech_assets_installed(_ locale: UnsafePointer<CChar>?) -> Int32 {
    let localeID = resolveLocaleID(locale)
    let semaphore = DispatchSemaphore(value: 0)
    var installed = false

    let task = Task {
        defer { semaphore.signal() }
        // Canonicalize both sides through Locale(...).identifier(.bcp47) so
        // hyphen/underscore forms (en-US vs en_US) cannot mismatch.
        let wanted = Locale(identifier: localeID).identifier(.bcp47)
        let installedLocales = await SpeechTranscriber.installedLocales
        installed = installedLocales.contains { $0.identifier(.bcp47) == wanted }
    }

    if semaphore.wait(timeout: .now() + 10) == .timedOut {
        task.cancel()
        return -1
    }
    return installed ? 1 : 0
}
