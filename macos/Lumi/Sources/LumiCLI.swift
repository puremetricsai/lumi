import Foundation

/// LumiCLI runs the `lumi` binary that ships inside this bundle.
///
/// Every question the app asks about Lumi goes through here. The app never opens
/// `lumi.db` and never parses `record.json`: `internal/store/migrations.go` owns
/// that schema and `internal/cli` owns that file format, and a second reader in
/// another language is the drift the repository's `CLAUDE.md` forbids. The
/// machine-readable CLI surface is the contract instead.
enum LumiCLI {
    /// The binary inside this bundle, never one found on PATH. A PATH lookup
    /// could resolve a different build than the one shipped here, and — more
    /// importantly — a child of this bundle is what keeps TCC attributing
    /// permission prompts to the app rather than to whoever launched it.
    static var executableURL: URL {
        Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/lumi")
    }

    /// The data directory the app and the CLI share. Passed explicitly on every
    /// invocation so both sides read the same store even when the app is
    /// launched by launchd with no environment at all.
    static var dataDirectory: String {
        Preferences.shared.dataDirectory
    }

    struct Result {
        let status: Int32
        let stdout: String
        let stderr: String

        var succeeded: Bool { status == 0 }
    }

    enum Failure: LocalizedError {
        case launch(String)
        case command(arguments: [String], status: Int32, stderr: String)
        case decode(String)

        var errorDescription: String? {
            switch self {
            case let .launch(message):
                return "Could not run the bundled lumi binary: \(message)"
            case let .command(arguments, status, stderr):
                let detail = stderr.trimmingCharacters(in: .whitespacesAndNewlines)
                let summary = detail.isEmpty ? "exit status \(status)" : detail
                return "lumi \(arguments.joined(separator: " ")) failed: \(summary)"
            case let .decode(message):
                return "Could not read lumi's output: \(message)"
            }
        }
    }

    /// run executes the binary and waits. It is `async` so that no caller can
    /// accidentally block the main thread on a subprocess; there is deliberately
    /// no synchronous variant.
    static func run(_ arguments: [String], includeDataDirectory: Bool = true) async throws -> Result {
        var argv = arguments
        if includeDataDirectory {
            argv += ["--data-dir", dataDirectory]
        }
        return try await withCheckedThrowingContinuation { continuation in
            DispatchQueue.global(qos: .utility).async {
                let process = Process()
                process.executableURL = executableURL
                process.arguments = argv
                let out = Pipe()
                let err = Pipe()
                process.standardOutput = out
                process.standardError = err
                process.standardInput = FileHandle.nullDevice
                do {
                    try process.run()
                } catch {
                    continuation.resume(throwing: Failure.launch(error.localizedDescription))
                    return
                }
                // Both pipes are drained concurrently, and only then is the
                // child waited on. Reading one to EOF first deadlocks whenever
                // the child fills the other: it blocks in write() on the pipe
                // nobody is reading, while the parent blocks waiting for an EOF
                // that will never come. Waiting before reading at all deadlocks
                // the same way as soon as either pipe's buffer fills.
                var outData = Data()
                var errData = Data()
                let group = DispatchGroup()
                let queue = DispatchQueue(label: "lumi.cli.read", attributes: .concurrent)
                queue.async(group: group) {
                    outData = out.fileHandleForReading.readDataToEndOfFile()
                }
                queue.async(group: group) {
                    errData = err.fileHandleForReading.readDataToEndOfFile()
                }
                group.wait()
                process.waitUntilExit()
                continuation.resume(returning: Result(
                    status: process.terminationStatus,
                    stdout: String(decoding: outData, as: UTF8.self),
                    stderr: String(decoding: errData, as: UTF8.self)))
            }
        }
    }

    /// json runs a `--json` command and decodes its stdout.
    ///
    /// A non-zero exit is not always a failure to decode: `permissions --json`
    /// prints a complete document and *then* exits non-zero when a request left
    /// something ungranted. So the payload is decoded first, and the status only
    /// consulted when there was nothing to read.
    static func json<T: Decodable>(_ type: T.Type, _ arguments: [String],
                                   includeDataDirectory: Bool = true) async throws -> T {
        let result = try await run(arguments, includeDataDirectory: includeDataDirectory)
        guard let data = result.stdout.data(using: .utf8), !data.isEmpty else {
            throw Failure.command(arguments: arguments, status: result.status, stderr: result.stderr)
        }
        do {
            return try decoder.decode(type, from: data)
        } catch {
            if !result.succeeded {
                throw Failure.command(arguments: arguments, status: result.status, stderr: result.stderr)
            }
            throw Failure.decode(error.localizedDescription)
        }
    }

    static let decoder: JSONDecoder = {
        let decoder = JSONDecoder()
        // Go's encoding/json writes RFC 3339 with nanoseconds, which
        // .iso8601 refuses. Both precisions are accepted here.
        let full = ISO8601DateFormatter()
        full.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        let plain = ISO8601DateFormatter()
        plain.formatOptions = [.withInternetDateTime]
        decoder.dateDecodingStrategy = .custom { decoder in
            let text = try decoder.singleValueContainer().decode(String.self)
            if let date = full.date(from: text) ?? plain.date(from: text) {
                return date
            }
            throw Failure.decode("unrecognised timestamp \(text)")
        }
        return decoder
    }()
}
