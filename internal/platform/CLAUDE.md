# internal/platform

`Validate` is the single gate that refuses anything but `darwin/arm64`; `internal/cli` calls it from
`PersistentPreRunE`, so every command inherits it. Native microphone capture additionally needs macOS 26+.

Keep the check here rather than per-command: a command that forgets it reaches the cgo bridge, where the
failure is a link or runtime error instead of a sentence the user can act on.
