import Foundation
import AVFoundation
import Speech

private func fail(_ errorMessage: UnsafeMutablePointer<UnsafeMutablePointer<CChar>?>?, _ message: String) -> UnsafeMutablePointer<CChar>? {
    errorMessage?.pointee = strdup(message)
    return nil
}

private func resolveLocaleID(_ locale: UnsafePointer<CChar>?) -> String {
    let requested = locale.map { String(cString: $0) } ?? ""
    return requested.isEmpty ? "en-US" : requested
}

private func makeTranscriber(localeID: String) -> SpeechTranscriber {
    SpeechTranscriber(
        locale: Locale(identifier: localeID),
        transcriptionOptions: [],
        reportingOptions: [],
        attributeOptions: []
    )
}

private func ensureAssets(for transcriber: SpeechTranscriber) async throws {
    if let request = try await AssetInventory.assetInstallationRequest(supporting: [transcriber]) {
        try await request.downloadAndInstall()
    }
}

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
            let transcriber = makeTranscriber(localeID: localeID)
            try await ensureAssets(for: transcriber)
            let analyzer = SpeechAnalyzer(modules: [transcriber])
            let audioFile = try AVAudioFile(forReading: URL(fileURLWithPath: path))

            let collector = Task { () -> String in
                var text = ""
                for try await result in transcriber.results {
                    text += String(result.text.characters)
                }
                return text
            }
            defer { collector.cancel() }

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
            try await ensureAssets(for: makeTranscriber(localeID: localeID))
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

@_cdecl("lumi_speech_assets_installed")
public func lumi_speech_assets_installed(_ locale: UnsafePointer<CChar>?) -> Int32 {
    let localeID = resolveLocaleID(locale)
    let semaphore = DispatchSemaphore(value: 0)
    var installed = false

    let task = Task {
        defer { semaphore.signal() }
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
