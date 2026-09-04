# Encrypting the data directory

Lumi's data directory holds full-display screenshots, audio, and a full-text index of every word
that has been on screen or spoken near the microphone. By default all of it is plaintext at mode
0700, which on macOS means **every process running as you can read it**.

The Storage settings toggle encrypts it. This document is the record of what that does, what it
does not do, and what it costs — the same role `docs/compress.md` plays for lossy re-encoding.

## The threat it was chosen for

A coding agent with shell access on the user's Mac. It can `grep` the index, open the screenshots,
and read months of work in one command — without any prompt, because nothing on macOS gates a
same-user process from `~/Library/Application Support`. TCC does not cover it, and the App Sandbox
would not either.

With encryption on:

| | |
| --- | --- |
| `lumi.db` | encrypted page by page through the `adiantum` SQLite VFS |
| every screenshot and audio file | `LUMIENC1` ‖ nonce ‖ AES-256-GCM, in place, name unchanged |
| the key | 32 random bytes in the login Keychain, ACL'd to Lumi's own code identity |
| `lumi search`, `lumi transcript`, `lumi transcribe` | refuse |
| `lumi mcp` | works — it decrypts in memory and writes only JSON-RPC frames |

Plaintext exists only inside a running `lumi` process.

## What it does not do

**It is not a boundary against a process that can execute Lumi's binary.** `lumi mcp` has to read
the key without prompting, so anything that can spawn it can drive JSON-RPC by hand and read the
index. The honest claim is narrower and still worth having: *ambient* access is gone. A `grep`, a
`find`, a stray `cat`, Spotlight, a Time Machine or cloud backup, another user account, a stolen or
resold disk — all of them now reach ciphertext. Reading the history takes going through the MCP
surface the user granted on purpose.

**It does not scrub what was already written.** Turning encryption on writes new files and unlinks
the old ones; the blocks the plaintext occupied stay recoverable on the SSD until they are
overwritten. That is what FileVault is for, and this change does not substitute for it.

**Adiantum is deterministic and unauthenticated.** Its own documentation says so: somebody holding
several snapshots of `lumi.db` — successive backups, say — learns exactly which 4 KiB blocks changed
between them and which were reverted, and the VFS makes no claim against tampering. For a local
index that is a fair trade. For a database synced to cloud backup it is weaker than "encrypted"
suggests.

**`record.log` and `record.json` stay plaintext.** The log carries application and window names, but
never extracted screen text or transcripts — those are logged as a character count. Sealing an
append-only log with a whole-file cipher would mean rewriting it on every line.

**Media is plaintext for a few seconds.** The native capture path writes a JPEG or WAV, Lumi reads
it for deduplication, OCR, and transcription, indexes the row, and only then seals the file. That
ordering is required by the never-lose-media rule: a file that is written and indexed is
recoverable, and a file encrypted before its row exists is not. There is a similar sub-second window
in `$TMPDIR` whenever OCR, transcription, or `lumi compress` needs to hand a path to a framework.
Those copies are removed on the way out, and a crash that skips that is swept up by the next
recording or conversion — but between the crash and the sweep they are readable.

## Losing the key destroys the data

There is no password, no recovery code, and no second copy. If the Keychain item goes — the Mac is
erased, the login keychain is reset, or the data folder is moved to another machine — the captured
history is gone. `lumi doctor` reports that state explicitly, because it is the one thing about the
index that cannot be inferred from the rows.

No recovery-key export is planned. One would be a second copy of the key sitting wherever the user
put it, which is the file the threat model above is about.

## How a conversion behaves

`lumi encrypt on` stores the key, seals the media, then converts the database. `lumi encrypt off`
converts the database, unseals the media, then deletes the key. Both orderings put the irreversible
step where a crash cannot strand anything: the key is written before the first file needs it and
deleted after the last file stops needing it.

Neither writes a journal or a progress file. **The headers are the record.** A media file either
starts with `LUMIENC1` or it does not; the database either starts with `SQLite format 3` or it does
not. A run that is killed halfway leaves a directory every reader handles correctly, and re-running
finishes it — skipping what is already done, with no risk of double-sealing.

A conversion refuses while a recording is in progress. That is enforced with a lock, not just a
check: every recorder holds `capture.lock` shared for its whole life and a conversion needs it
exclusively, so neither can start underneath the other. It also takes `compress.lock`, since both
rewrite media in place.

Two things a conversion will not do quietly. It never deletes the key while any file is still
sealed — that file would be gone, so it reports the failure and keeps the key. And it never exits
zero having left plaintext media behind, because converting the database is what makes every status
surface say "encrypted".

## The Keychain item, and why it is the legacy one

Two macOS keychains could hold this key, and they offer different things:

- The **data-protection keychain** refuses other processes outright, with no prompt. Its access
  control is by application-identifier entitlement, so it needs a real signing identity — measured
  from an ad-hoc build, `SecItemAdd` fails with `errSecMissingEntitlement` (-34018). Every
  development build would be unable to store a key at all.
- The **legacy file-based keychain** takes a `SecAccess` ACL naming a specific binary, which is the
  per-binary control this feature wants, and it works ad-hoc. It ignores `kSecAttrAccessible*`.

Lumi uses the legacy keychain. Measured with an ad-hoc signature: Lumi reads its own key with no
prompt, and `/usr/bin/security` asking for the *data* blocks on a user prompt. Reading only the
*attributes* is ungated for anybody, which is exactly why the "is encryption on" check asks for
attributes and never for data — refusing `lumi search` costs nobody a Keychain dialog.

The key is stored under a fixed account name, not the data directory's path. Keying it on the path
would mean Storage settings' **Choose…** button silently produced a store nothing could decrypt.

**Development cost, not a bug.** A local build is ad-hoc signed, so its code identity changes every
rebuild and the ACL trusts a binary that no longer exists. Expect a Keychain prompt after
`./scripts/restart-lumi-app.sh`. This is the same class of problem as the TCC grants that a rebuild
already destroys (`macos/CLAUDE.md`), and it has the same non-fix: sign development builds with the
Developer ID, or accept the prompt.

Rotating the release signing certificate invalidates the ACL for every existing user, exactly as it
invalidates their TCC grants. `docs/signing-and-notarization.md` owns that half.

## Reading a screenshot yourself

`lumi reveal <event-id>` decrypts one event's media and opens it in QuickLook. The decrypted copy is
0600, lives in `$TMPDIR`, and is deleted the moment the preview window closes — `qlmanage -p` blocks
for exactly that long, which `open -W` does not, because Preview is usually already running.

This is a second way to get captured content out of the CLI, and it is deliberate rather than an
oversight: media that can never be looked at is media the user cannot audit. The bound on its
lifetime is what keeps it from being an id-enumeration hole.
