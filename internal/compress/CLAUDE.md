# internal/compress

Re-encodes indexed media in place behind `lumi compress`: screenshots to HEIC, audio to lossless FLAC,
leftovers from an interrupted run reconciled, then `VACUUM`. Pure Go with no build tags — the encoders are
injected as `ImageTranscoder`/`AudioTranscoder`, so the whole sequence tests anywhere with fakes and the
concrete `NativeImages`/`NativeAudio` are thin adapters over `internal/macosnative`. No scheduler, matching
`internal/retention`. Nothing here deletes an event; prune decides what history to keep, compress decides
how densely to keep it.

## Invariants

- **Files are replaced before rows are repointed — the exact inverse of `internal/retention`'s
  rows-before-files rule, and both are correct for what they protect.** Prune deletes the row first because
  an orphaned file is recoverable and a row naming media that no longer exists is not. Compress writes and
  verifies first because here the unrecoverable state is a row naming media that does not exist *yet*. The
  full sequence is: encode → verify → fsync the file **and its parent directory** → conditional UPDATE →
  delete the original. `internal/retention/CLAUDE.md` describes the pair together; do not "fix" either to
  match the other.
- **Nothing is deleted that was not first verified against what it replaces.** A silent encoder failure
  that then deletes the source is the one bug in this feature that destroys data, so an encoder's own
  success return is never enough: the bridge reopens what it wrote, because a truncated file still
  finalises. Audio is exact — decode both sides through the same decoder and compare a hash of the samples,
  which FLAC being lossless makes free of thresholds. Images clear three independent gates (dimensions,
  PSNR, histogram similarity), independent because a colour-space or channel-order mistake can pass one and
  fail another. Encode and verify failures are counted apart: one means a broken input, the other means an
  encoder producing something wrong while reporting success.
- **The directory `fsync` is load-bearing, so its failure is fatal for that file.** Without it the new
  file's *name* may not survive a power loss that the committed row update does, which reintroduces exactly
  the case the ordering prevents. Claiming a step is required and then continuing without it would be
  incoherent — a failed sync unlinks the destination and keeps the original.
- **The crash-safety claim holds because exactly one compressor runs at a time, and it is only true with
  that scope.** A lost compare-and-swap makes this package delete the file it just wrote; with a second
  compressor running, that file can be the one the winner's row now names, which is unrecoverable. The
  single-instance lock lives in `internal/cli` (see its `CLAUDE.md`). State the scope wherever the claim is
  repeated.
- **`reconcile` is sibling-scoped and must never become a general orphan sweep.** Captured media exists on
  disk *before* its row does — a frame is written, compared, then inserted; a WAV exists for a whole chunk
  plus transcription latency before its `Insert` — so removing any unreferenced file would delete media the
  recorder had not indexed yet. That is "never lose captured media" broken directly, and it is why the
  general sweep stays behind `prune --all`'s irreversible confirmation. Only a file whose extension-swapped
  sibling is named by a row may be touched; freshly captured media has no such sibling, so it is
  untouchable by construction rather than by a check. `TestReconcileNeverTouchesMediaThatHasNoIndexedSibling`
  pins it.
- **A leftover whose row's media is *gone* is adopted, not deleted.** Media is flushed with a full barrier
  while SQLite's WAL commit by default is not, so a power loss can drop the committed row update while the
  unlink of the original persists. Deleting the survivor would destroy the last copy; repointing the row at
  it turns that tail case into a repair. It is checked for decodability first, which is the weakest check
  in the package and is applied only where the alternative is losing the data.
- **Step order: passes → reconcile → vacuum, and each boundary is load-bearing.** Reconcile builds its
  reference set from `AllEvents` *at call time*; a set assembled before the passes would not name anything
  they had just written, so reconcile would delete the entire run's output. `VACUUM` is last because it
  reclaims the `events_au` churn this run created and needs an exclusive lock nothing else in the run
  should contend for.
- **Rows are repointed one at a time in autocommit, never inside one transaction.** Crash safety here is
  per *file* — that is the whole point of the ordering — and a transaction spanning N files would roll back
  N committed repointings whose originals had already been unlinked, converting the recoverable case into
  the unrecoverable one.
- **Eligibility is a case-insensitive whitelist, not a blacklist.** A format this build does not know is
  counted as skipped and left alone, so a container added later is never fed to the wrong encoder. Every
  row a pass looks at lands in exactly one counter, so a run that compressed nothing still says why.
- **Idempotence is by extension, and it does not cover crashes.** A row whose path ends in `.heic` or
  `.flac` is done, which needs no state column and no migration — but a crash leaves a file no rerun would
  notice, which is what `reconcile` is for. Do not conflate the two.
- **The image comparison is native and cannot reuse `internal/capture/compare.go`.** That package decodes
  with `image.Decode` and `_ "image/jpeg"`, and the Go standard library has no HEIC decoder, so it cannot
  open the file this verification exists to check. Its histogram helpers are also unexported and belong to
  `FrameComparer`'s per-display state machine. The native comparison mirrors its *shape* — 16 bins per
  channel — so the two numbers stay commensurable, and that is as far as the sharing goes.
- **A per-file failure never aborts the run.** It is counted, logged, its destination is unlinked, and the
  next file is attempted. Only a store error or a cancelled context stops a pass.

## Thresholds

`DefaultQuality` (0.60), `DefaultMinPSNRDB` (30) and `DefaultMinHistogramSimilarity` (0.95) are measured,
not guessed: over 309 frames sampled across a real index, HEIC q60 gave 2.58× with a worst-case PSNR of
39.4 dB and a worst-case histogram similarity of 0.989. The floors are encoder-sanity gates sitting well
below anything the corpus contains — if a real frame ever fails one, the answer is to raise `--quality`,
not to lower the gate. A synthetic test fixture is not evidence about them: high-frequency noise measures
23.6 dB at the same setting, which is why `writeTestJPEG` in `internal/macosnative` is shaped like a
desktop.
