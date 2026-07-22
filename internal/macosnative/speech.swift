import Foundation

/// Trivial liveness probe proving the Swift static archive links into the Go
/// binary and is callable across the cgo boundary. Returns a heap string the Go
/// side frees with C.free (see nativeString in native.go).
@_cdecl("lumi_speech_ping")
public func lumi_speech_ping() -> UnsafeMutablePointer<CChar>? {
    return strdup("pong")
}
