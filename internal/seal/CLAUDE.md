# internal/seal

Encrypts captured media files in place: `"LUMIENC1"` ‖ 12-byte nonce ‖ AES-256-GCM. Pure Go, stdlib
only, no build tag — like `internal/wav` and `internal/transcript`, so the rules it enforces are
exercisable anywhere. It knows nothing about screenshots, WAVs, the database, or the filesystem
layout.

- **The magic header is the load-bearing decision, not the cipher.** Every reader detects for itself
  whether a file is sealed, so a directory half-way through a conversion is a *correct* state rather
  than a broken one: `lumi encrypt` can be killed and re-run, it skips what it already did, and
  nothing has to write down where it got to. There is no journal because the files are the journal.
  Sealing an already-sealed file and unsealing an unsealed one are both no-ops for the same reason.
- **`Key`'s zero value is a working pass-through, and that is why this is a type.** Every caller —
  capture, compress, the backfill, `reveal` — is written once and behaves correctly whether or not
  encryption is on. There is no encrypted variant of any pipeline. `Key(nil).TempCopy(p)` returns `p`
  itself and a cleanup that does nothing, so no call site needs a branch.
- **A keyed reader still reads plaintext.** Both kinds are on disk at once during a conversion, and
  for the seconds between a capture landing and being sealed. A reader that insisted on ciphertext
  would fail on exactly the files it is most important not to lose.
- **Sealing never changes a file's name.** `internal/compress` classifies work by extension,
  `capture.ReadAudioEnvelope` dispatches on `.wav`, and `internal/compress` pairs a file with an event
  by swapping one. A sealed screenshot is still `.jpg`.
- **Two keys are derived from one stored master** (`DeriveDB`, `DeriveMedia`, HKDF-SHA256). One secret
  is stored and two are used, so a weakness or a format change in either half cannot reach the other.
  Derivation is deterministic, or an upgrade could not read yesterday's data.
- **A decrypt failure never names the cipher error.** On a wrong key it is indistinguishable from a
  tampered file, and claiming corruption when the key is simply wrong sends the user to recover data
  that is intact.
- **`TempCopy` writes to `$TMPDIR`, never beside the media.** `internal/compress`'s reconcile walk
  reads an unrecognised sibling as an orphaned encode, and `internal/retention`'s `--all` sweep
  deletes any unreferenced file it finds — a plaintext temporary in a media directory is data loss
  waiting for the next `prune --all`. It exists at all because the native APIs (Vision, SpeechAnalyzer,
  the HEIC and FLAC encoders) take paths rather than readers.
- **`ScratchSuffix` is exported because two other packages have to recognise it.** A crash mid-seal
  leaves one beside the media, where `internal/compress`'s reconcile would read it as an orphaned
  encode and `lumi encrypt`'s resume would try to convert it. Naming it here is what stops either
  guessing.
- **`SealFile` replaces through a sibling temporary, fsyncing the file and the directory before the
  rename.** It is overwriting the only copy of a captured file.
- **`SealInto` exists for `internal/compress` alone.** Sealing in place would mean the destination
  existed unsealed first, which is the one moment that ordering cannot have.
