import Foundation
import AVFoundation
import CoreMedia
import Speech

private func fail(_ errorMessage: UnsafeMutablePointer<UnsafeMutablePointer<CChar>?>?, _ message: String) -> UnsafeMutablePointer<CChar>? {
    errorMessage?.pointee = strdup(message)
    return nil
}

private func resolveLocaleID(_ locale: UnsafePointer<CChar>?) -> String {
    let requested = locale.map { String(cString: $0) } ?? ""
    return requested.isEmpty ? "en-US" : requested
}

// makeTranscriber requests per-run timing and confidence. Both are free — they
// annotate results the transcriber already produces — and asking for them does
// not change the transcript: the spike that preceded this code confirmed the
// concatenated text is byte-identical with and without attributeOptions.
//
// reportingOptions stays empty deliberately. `.volatileResults` would emit
// partial hypotheses whose ranges overlap, and every caller here assumes
// results tile the timeline monotonically.
private func makeTranscriber(localeID: String) -> SpeechTranscriber {
    SpeechTranscriber(
        locale: Locale(identifier: localeID),
        transcriptionOptions: [],
        reportingOptions: [],
        attributeOptions: [.audioTimeRange, .transcriptionConfidence]
    )
}

private func ensureAssets(for transcriber: SpeechTranscriber) async throws {
    if let request = try await AssetInventory.assetInstallationRequest(supporting: [transcriber]) {
        try await request.downloadAndInstall()
    }
}

// SpeechRun is one timed span inside a segment — in practice a word. Runs
// without a resolvable time range are dropped, so the runs of a segment do not
// necessarily concatenate back to its text; the segment's own `text` is always
// the authority.
private struct SpeechRun: Encodable {
    let startMS: Int64
    let endMS: Int64
    let text: String
    let confidence: Double?

    enum CodingKeys: String, CodingKey {
        case startMS = "start_ms"
        case endMS = "end_ms"
        case text
        case confidence
    }
}

private struct SpeechSegment: Encodable {
    let startMS: Int64
    let endMS: Int64
    let text: String
    let confidence: Double?
    let runs: [SpeechRun]

    enum CodingKeys: String, CodingKey {
        case startMS = "start_ms"
        case endMS = "end_ms"
        case text
        case confidence
        case runs
    }
}

private struct TranscriptionPayload: Encodable {
    let text: String
    let segments: [SpeechSegment]
}

private func millis(_ time: CMTime) -> Int64? {
    guard time.isValid, !time.isIndefinite else { return nil }
    let seconds = CMTimeGetSeconds(time)
    guard seconds.isFinite else { return nil }
    return Int64((seconds * 1000).rounded())
}

// makeSegment converts one transcriber result into a segment.
//
// The span comes from the union of the result's run intervals, not from
// `result.range`, and that distinction is load-bearing. Measured on real
// capture, a result reporting 3000..9660 ms held a single word occupying
// 3000..3660 ms — `result.range` extends to the finalization boundary, roughly
// ten times the actual speech. Attributing overlap from the wider span marks
// neighbouring microphone speech as coinciding with machine audio and penalises
// it for simultaneous speech that never happened.
//
// `result.range` is the fallback only when a result carries no runs at all. That
// case is not a defect: it is an empty-text result, which is itself the useful
// signal "audio was present across this span but no words resolved".
private func makeSegment(_ result: SpeechTranscriber.Result) -> SpeechSegment {
    let attributed = result.text
    var runs: [SpeechRun] = []
    var confidences: [Double] = []

    for run in attributed.runs {
        guard let range = run.audioTimeRange,
              let start = millis(range.start),
              let end = millis(CMTimeRangeGetEnd(range)) else { continue }
        let confidence = run.transcriptionConfidence
        if let confidence = confidence { confidences.append(confidence) }
        runs.append(SpeechRun(startMS: start, endMS: end,
                              text: String(attributed[run.range].characters),
                              confidence: confidence))
    }

    let start = runs.map(\.startMS).min() ?? millis(result.range.start) ?? 0
    let end = runs.map(\.endMS).max() ?? millis(CMTimeRangeGetEnd(result.range)) ?? start
    let mean = confidences.isEmpty
        ? nil
        : confidences.reduce(0, +) / Double(confidences.count)

    return SpeechSegment(startMS: start, endMS: end,
                         text: String(attributed.characters),
                         confidence: mean, runs: runs)
}

// runTranscription is the single ASR path. Both C entry points go through it, so
// the flat transcript and the timed segments can never disagree.
//
// No AnalysisContext is set. Lumi shipped a custom-vocabulary feature that
// supplied `context.contextualStrings = [.general: terms]` here, and it was
// removed once measurement showed Apple ignores it: on macOS 26.5.2, replaying
// fixed WAVs with and without terms produced byte-identical transcripts, for
// both SpeechTranscriber and DictationTranscriber, whether the context was set
// via `setContext` or passed at analyzer construction. The terms reached the
// framework — reading `analyzer.context.contextualStrings` back returned them
// exactly — so the plumbing was correct and the bias simply never applied.
// Re-adding a term list means re-establishing that it changes a transcript
// first; `DictationTranscriber.ContentHint.customizedLanguage` is the remaining
// documented path, and it costs a large drop in how much speech is recovered.
private func runTranscription(path: String, localeID: String) async throws -> TranscriptionPayload {
    let transcriber = makeTranscriber(localeID: localeID)
    try await ensureAssets(for: transcriber)
    let analyzer = SpeechAnalyzer(modules: [transcriber])
    let audioFile = try AVAudioFile(forReading: URL(fileURLWithPath: path))

    let collector = Task { () -> TranscriptionPayload in
        var text = ""
        var segments: [SpeechSegment] = []
        for try await result in transcriber.results {
            // No separator. Runs carry their own leading whitespace, so plain
            // concatenation is what produces readable prose — and it is the
            // byte-for-byte shape every already-indexed row was written with.
            // Joining with a space would re-index the whole corpus for reasons
            // unrelated to this feature.
            text += String(result.text.characters)
            segments.append(makeSegment(result))
        }
        return TranscriptionPayload(text: text, segments: segments)
    }
    defer { collector.cancel() }

    _ = try await analyzer.analyzeSequence(from: audioFile)
    try await analyzer.finalizeAndFinishThroughEndOfInput()
    return try await collector.value
}

// bridgeTranscription runs body under the caller's timeout and hands back a
// strdup'd C string, matching the error vocabulary both entry points shipped
// with.
private func bridgeTranscription(
    localeID: String,
    timeout: Double,
    _ errorMessage: UnsafeMutablePointer<UnsafeMutablePointer<CChar>?>?,
    _ body: @escaping @Sendable () async throws -> String
) -> UnsafeMutablePointer<CChar>? {
    let semaphore = DispatchSemaphore(value: 0)
    var produced: String?
    var errorText: String?

    let task = Task {
        defer { semaphore.signal() }
        do {
            produced = try await body()
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
    guard let produced = produced else {
        return fail(errorMessage, "transcribe: no result produced")
    }
    return strdup(produced)
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

    return bridgeTranscription(localeID: localeID, timeout: timeout, errorMessage) {
        try await runTranscription(path: path, localeID: localeID).text
    }
}

@_cdecl("lumi_transcribe_audio_segments_json")
public func lumi_transcribe_audio_segments_json(
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

    return bridgeTranscription(localeID: localeID, timeout: timeout, errorMessage) {
        let payload = try await runTranscription(path: path, localeID: localeID)
        let encoder = JSONEncoder()
        let data = try encoder.encode(payload)
        guard let json = String(data: data, encoding: .utf8) else {
            throw NSError(domain: "LumiSpeech", code: 1,
                          userInfo: [NSLocalizedDescriptionKey: "encode transcription payload as UTF-8"])
        }
        return json
    }
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
