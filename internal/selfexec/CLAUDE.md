# internal/selfexec

Watches the executable a long-lived process was started from and replaces the process image when that file
changes. `NewWatcher()` stamps the binary, `Watcher.Changed()` reports whether it moved, `Exec(path)` becomes
the new build. It has no dependencies on anything else of Lumi's, and it decides only *whether* the binary
moved — *when* it is safe to act on that is the caller's question, and on a JSON-RPC stream a sharp one
(`internal/mcp/CLAUDE.md`).

It exists for `lumi mcp`. An agent launches that server once and keeps it for the whole session, so an
upgrade that replaces the file on disk changes nothing about the running process — on Unix the old image
stays mapped until it exits — and the client will not relaunch it mid-session. No MCP transport or protocol
revision addresses that, including the 2026-07-28 stateless revision: it is process lifecycle, and on stdio
the client owns it.

## Invariants

- **`Exec` is `syscall.Exec`, never a fork-and-exit.** Replacing the image in place keeps the pid and, the
  property everything rests on, file descriptors 0, 1 and 2. The MCP client holds the other end of those
  pipes, so from its side nothing happened: same process, same streams, newer code. A fork would hand the
  child new descriptors and orphan the client's connection. It returns only on failure, and a failed
  `execve` leaves the current image running and intact — so the caller's correct response is to log it and
  keep serving as the build it already is.
- **The stamp is of the *resolved* target, but the path to exec stays unresolved.** A packaged install is
  now `/Applications/Lumi.app/Contents/MacOS/lumi`, which install.sh replaces wholesale: no symlink is
  involved, the stamp of the resolved target simply differs afterwards, and the two halves of this rule
  agree trivially. The rule exists for the case where they do not. Reached through a symlink — a
  developer's own — the link keeps its identity across an upgrade
  while the file behind it changes, so stamping the resolved target is what makes the upgrade visible at
  all, and exec'ing the unresolved path is what makes the *next* one visible too
  (`internal/mcpsetup/CLAUDE.md`). Either way it is the path `lumi mcp setup` baked into the client's
  config.
- **A binary that cannot be stat'd is not a change.** The file is briefly absent mid-install, and calling
  that an upgrade would exec a path that does not exist yet — killing a working server to chase a build
  that is not there. `Changed()` returns false on any stat error; the next check picks the real upgrade up.
- **`NewWatcher` prefers an absolute `os.Args[0]` over `os.Executable()`.** `lumi mcp setup` writes an
  absolute argv[0] into every client config (`internal/cli/CLAUDE.md`), and that path is the stable
  symlink, while `os.Executable()` reports the resolved target an upgrade abandons rather than rewrites.
  A relative argv[0] names nothing stable — the server's working directory is wherever the client launched
  it — so that case falls back to `os.Executable()`.
- **The non-Unix `Exec` is a build-time stub, not a fallback.** There is no portable way to replace a
  process image, and the alternatives do not preserve the client's pipes. The binary refuses to run off
  `darwin/arm64` anyway (`internal/platform`); the stub only keeps cross-compilation and vet working.
