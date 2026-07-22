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

/// Transcribes a WAV file with on-device SpeechAnalyzer. Returns a heap C string
/// (freed by Go via C.free) on success. On ANY failure returns NULL and sets
/// *error_message — never an empty string masquerading as a transcript.
@_cdecl("lumi_transcribe_audio_string")
public func lumi_transcribe_audio_string(
    _ audioPath: UnsafePointer<CChar>?,
    _ locale: UnsafePointer<CChar>?,
    _ errorMessage: UnsafeMutablePointer<UnsafeMutablePointer<CChar>?>?
) -> UnsafeMutablePointer<CChar>? {
    guard let audioPath = audioPath else {
        return fail(errorMessage, "transcribe: nil audio path")
    }
    let path = String(cString: audioPath)
    let requestedLocale = locale.map { String(cString: $0) } ?? ""
    let localeID = requestedLocale.isEmpty ? "en-US" : requestedLocale

    let semaphore = DispatchSemaphore(value: 0)
    var resultText: String?
    var errorText: String?

    Task {
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
            // happens on first use of a locale.
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

    semaphore.wait()

    if let errorText = errorText {
        return fail(errorMessage, errorText)
    }
    guard let resultText = resultText else {
        return fail(errorMessage, "transcribe: no result produced")
    }
    return strdup(resultText)
}
