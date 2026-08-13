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
- **The crash-safety claim holds because exactly one compressor *process* runs at a time, and it is only
  true with that scope.** A lost compare-and-swap makes this package delete the file it just wrote; with a
  second compressor running, that file can be the one the winner's row now names, which is unrecoverable. The
  single-instance lock lives in `internal/cli` (see its `CLAUDE.md`). State the scope wherever the claim is
  repeated.
  - **"One compressor" is a statement about processes, and workers inside one pass are deliberately not one.**
    The hazard is two agents deriving the same destination, and two *processes* can, because each builds its
    own candidate list and can therefore both hold the same row. Workers cannot: the list is built once, each
    item is dispatched to exactly one worker, and `findConflicts` has already skipped every row sharing a
    source or a derived destination with another. So the property concurrency needs was established by
    preflight before any of this was parallel — it is not a new assumption, which is exactly why the lock
    still cannot be relaxed to cover it. A lost compare-and-swap inside a pass therefore still means a
    genuinely external writer, and `Raced` still describes it correctly.
    **Per-file ordering is untouched**: one worker carries one file through encode → verify → `fsync` → CAS →
    unlink, and that ordering never depended on what other files were doing. What does change is the cost of
    interruption — cancelling abandons up to `workers` files instead of one, each left exactly as an
    interrupted sequential run leaves its single file, and all of them repaired by reconcile's one sibling
    rule. More of the same recoverable state, not a new kind.
    **`replaceOne` holds the sequence and `PassResult.record` holds the accounting**, split for this: with
    several files in flight, "every file lands in exactly one counter" is only checkable if one place touches
    the counters. One mutex covers the counters, the progress tracker and the first error, which is free
    against ~100 ms of encoding per file.
    **The concurrency is pinned by a rendezvous, not a sleep.** `TestCompressKeepsSeveralFilesInFlight`
    blocks every encode until `workers` have arrived, so it can only pass if they genuinely overlap and it
    fails with a diagnosis rather than hanging if a future change serialises the pass. The test fakes are
    mutex-guarded for the same reason, and a test needing one file of several to fail must use
    `fakeImages.failFor` rather than flipping a shared `err` from `observe` — which one encode reads which
    value is undefined.
- **Every pass is confined to `Options.MediaDirs`, and a run naming media outside them fails before it
  touches anything.** `media_path` is stored absolute, so pointing `--data-dir` at a *copy* of an index
  gives you the copy's database and the original's files — and this is the only command that rewrites and
  deletes what it reads. That is not hypothetical: it destroyed 3,300 originals in a live index during this
  feature's own verification, silently, while reporting success. `checkRoots` fails the whole run rather
  than skipping the offending rows, because media outside the roots means the caller is not looking at the
  index they think they are. Note the asymmetry that allowed it: reconcile was always scoped to
  `MediaDirs`; the passes were not.
- **One-to-one path ownership is checked, not assumed, on three axes.** `events.media_path` has no
  uniqueness constraint, and destinations are derived by swapping the extension, so all of: two rows naming
  one file (the first replacement deletes the second's media), a row whose destination another row already
  holds (the encode overwrites it — the FLAC writer truncates an existing destination outright), and **two
  rows deriving the same destination** break the sequence. `findConflicts` skips and counts those rows
  instead. None arises in an index Lumi wrote, which is exactly why they need a check rather than a comment.
  - **The third axis is the one with no filesystem behind it, and it was missed once.** Neither destination
    exists when the run starts, so `os.SameFile` — which decides every other comparison here — has nothing
    to stat, and the collision is invisible to any check made against what is on disk. `.jpg` and `.jpeg`
    beside one another both derive `frame.heic`: the second encode overwrites the first, both originals are
    then unlinked, and the run reports success having destroyed one picture and pointed both rows at the
    other. Reconcile cannot repair it either — with both originals gone there is no leftover to adopt and
    nothing to compare. So that axis compares strings, and both normalisations are load-bearing, for
    different reasons. The string is the **resolved** one — `resolvedPath` resolves the existing *parent* of
    an absent final component, and without that `/a/frame.jpg` and `/b/frame.jpeg` with `/b → /a` key
    differently while colliding on disk. Resolution cannot produce a false pair: two paths resolving to one
    string *are* one destination, so it is not a proxy for the collision, it is the collision. Only the
    **case fold** over-pairs, and that is the safe direction — on a case-sensitive volume it can pair rows
    that would not have collided, at a cost of skipping compression.
  - **The key's fallback to the unresolved path is safe only because `checkRoots` fails the run, and that
    borrowed guarantee is why `checkRoots` may not be softened into skipping rows.** If one of two colliding
    rows resolved and the other did not, they would key differently and the collision would be missed — the
    exact loss above. It cannot happen today because `checkRoots` already calls `resolvedPath` on every
    candidate's source *and* derived destination and returns the error, so two candidates either both
    resolve or the run never starts. "Why abort a whole run over one bad path — just skip it" is the obvious
    later improvement, and it silently re-arms this. A *non*-candidate partner that fails to resolve can
    miss, but nothing is written for it this run, and by the run in which it becomes eligible its
    destination exists on disk and the `os.SameFile`-backed axis catches it.
  - **Resolution is best-effort, and everything it misses routes to that same hard failure.**
    `EvalSymlinks` does not resolve APFS firmlinks, so `/Users/x/media/f.jpg` and
    `/System/Volumes/Data/Users/x/media/f.jpg` are one file under two spellings that never compare equal.
    It cannot reach this axis: `MediaDirs` come from config as `/Users/...`, so a firmlink-spelled row falls
    outside the resolved roots and `checkRoots` refuses the run. Fails closed.
- **`reconcile` is sibling-scoped and must never become a general orphan sweep.** Captured media exists on
  disk *before* its row does — a frame is written, compared, then inserted; a WAV exists for a whole chunk
  plus transcription latency before its `Insert` — so removing any unreferenced file would delete media the
  recorder had not indexed yet. That is "never lose captured media" broken directly, and it is why the
  general sweep stays behind `prune --all`'s irreversible confirmation. Only a file whose extension-swapped
  sibling is named by a row may be touched; freshly captured media has no such sibling, so it is
  untouchable by construction rather than by a check. `TestReconcileNeverTouchesMediaThatHasNoIndexedSibling`
  pins it. **The pairing folds case, because the passes do**: `classify` matches on `lowerExt` and always
  writes the lower-case extension, so a row naming `frame.JPG` is compressed to `frame.heic` and an
  exact-match key would find no owner for the leftover — which then leaks on that run and every run after
  it. A false match on the *skip* test only leaves a file alone, but the **owner lookup is not symmetric
  with it**: two rows folding to one key would resolve by last-writer-wins, and `settle` handed the
  surviving row stats that row's media and deletes a leftover that may be the other row's only copy. So a
  collided key owns nothing and its leftover is left alone with a log line. Nothing in a leftover's name
  says which of two rows wrote it, and this package does not delete what it cannot attribute.
- **"Could not stat" is not "is gone", and reconcile must keep them apart.** Routing every stat error to
  the adopt branch is the one single-process path in this package that destroys data: a transient EACCES or
  EIO on a *present* original adopts the unverified leftover, and the next run then sees the original as an
  unreferenced sibling of a referenced file and deletes the only copy that was ever verified. Adopt only on
  `os.IsNotExist`; anything else leaves both files alone and logs.
- **A leftover whose row's media is *gone* is adopted, not deleted.** Media is flushed with a full barrier
  while SQLite's WAL commit by default is not, so a power loss can drop the committed row update while the
  unlink of the original persists. Deleting the survivor would destroy the last copy; repointing the row at
  it turns that tail case into a repair. It is checked for decodability first, which is the weakest check
  in the package and is applied only where the alternative is losing the data.
- **A pass reports where it has got to, because a silent one is indistinguishable from a hung one and
  interrupting costs a whole pass rather than a file.** Everything here is accounted for in counters that are
  returned at the end, and the logger is otherwise reached only by a failure — so a full-index run, at
  roughly 100 ms per HEIC encode plus two `fsync`s per replacement, printed nothing at all for twenty
  minutes. That was measured, not guessed: 623 files took 75 s wall (59% CPU; the rest is the barrier), which
  puts ~9,000 pending files near 20 minutes. The reason this is a correctness concern and not a cosmetic one
  is the step order above: screens run before audio, and a cancelled pass returns before the next one starts,
  so a user who reads the silence as a hang and presses Ctrl-C forfeits the *audio* pass entirely — the
  lossless one, which measures 7x. An interrupted index shows this exactly, with the screen rows part
  converted, every audio row still `.wav`, and no leftovers at all because the cancellation was clean.
  - **The numerator counts files the pass has finished with, not files it replaced.** Reporting successes
    would stall the line at `done=0` for a run whose every file fails verification — a `--min-psnr` above
    what the corpus can meet, or a broken encoder — which is exactly the run a user reads as a hang and
    interrupts, at the cost described above. Liveness is this line's whole job; which counter each file
    landed in is the summary's. `TestCompressProgressAdvancesThroughAPassThatReplacesNothing` pins it.
  - **The denominator counts files, and getting an honest one is why the gates run in their own phase.**
    `selectWork` applies `done`, `conflicted`, `classify` and `stat` to every candidate row and returns the
    survivors; `runPass` then encodes that list. Counting *rows* instead would have been free, and would have
    lied: a real index is dominated by aged-out and already-compressed rows costing microseconds each, so
    `6024 of 13211 rows` reads as 45% of a run that has encoded 108 files out of 223. The alternative —
    counting eligible files in a second loop of the same gates — is the duplicated-rule drift this file warns
    about everywhere else, and it would be invisible to both loops' tests.
    **The split moves no ordering.** Every gate in `selectWork` is a read-only check, and the per-file
    sequence is untouched and still strictly one file at a time. The one behavioural difference is that a
    file something else deletes *during* the run now counts as an encode failure rather than a missing file:
    a pre-existing race that this can only mislabel, never mishandle. `TestCompressProgressCountsFilesNotRows`
    pins the count against one row rejected by each of the four gates.
  - **Rate-limited by elapsed time, not by file count.** The per-file cost spans two orders of magnitude
    between a 5K frame and a silent audio chunk, so a count-based trigger reports at an unpredictable cadence
    and can bury the warnings the logger is actually there for. `progressInterval` is a package var only as a
    test seam — the default must leave a short run exactly as quiet as it was before any of this existed,
    which is what makes the reporting path otherwise unreachable in a test.
  - **A pass that was reporting also reports where it stopped, including a cancelled one.** That line is the
    only account of how far a run a user gave up on had actually got, and it fires from a `defer` so the
    early return on `ctx.Err()` cannot skip it.
  - **`VACUUM` announces itself instead of reporting progress**, because SQLite offers none. It is the last
    step, it rewrites the whole file under an exclusive lock, and it runs after the passes have stopped
    talking and before the summary is printed — the stretch most easily read as a hang. A disabled or
    dry-run vacuum says nothing; the line is about a lock that is actually about to be held.
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
  - **Past the compare-and-swap this stops being a policy and becomes a requirement, and the handling
    inverts.** Before the swap a failure unlinks the destination; after it the row is committed and names
    that file, so unlinking it would be the unrecoverable case rather than the cleanup. Nothing after the
    swap may return either: a single `EIO` while measuring or unlinking one original would abandon the rest
    of the pass, the other pass, and reconcile — stranding the originals of every file already replaced,
    none of which is reclaimed until somebody runs compress again. Both are logged and stepped over; a
    stranded original is exactly reconcile's owner-is-present case.

## Concurrency

`MaxDefaultWorkers` (8) is measured, not guessed. Over 180 real 3456x2234 frames on an 18-logical-core
machine with 6 performance cores, wall time went 37.6 s at one worker → 22.5 s at 2 → 14.4 s at 4 → 11.4 s
at 6 → 9.9 s at 8, and then stopped improving: 10.1 s at 10, 9.4 s at 12, 9.3 s at 18. The cap keeps 3.8x of
the available 4.0x and declines the rest because it costs memory for nothing — peak RSS was 1.37 GB at one
worker, 2.02 GB at 8 and 2.72 GB at 18, roughly 93 MB per additional file in flight, since verifying one
image holds two full RGBA buffers plus both decoded images.

The default is `min(GOMAXPROCS, MaxDefaultWorkers)`; `--workers 1` restores the strictly sequential
behaviour, and a negative count is refused rather than read as unset, for the same reason `--quality 0` is.
`MaxDefaultWorkers` caps only what this package picks on the caller's behalf: **an explicit count is
honoured however large, deliberately.** Clamping a number the caller typed is the same quiet disagreement as
silently correcting `--quality` — the flag would name a concurrency the run did not use — and the ceiling to
clamp it to would have to be invented rather than measured. The cost of an absurd one is memory at the ~93 MB
per file in flight above, which is the user's own override of a measured default. The one bound that is
applied is against the work: `replaceFiles` starts no more workers than there are files, since a goroutine
past `len(items)` can only ever observe a closed channel.

The large baseline is worth noticing separately: 1.37 GB at one worker is mostly `AllEvents` being
materialised twice per run, with every row's OCR text. That is the thing to fix if this ever needs to be
cheaper — not the worker count.

## Thresholds

`DefaultQuality` (0.60), `DefaultMinPSNRDB` (30) and `DefaultMinHistogramSimilarity` (0.95) are measured,
not guessed: over 309 frames sampled across a real index, HEIC q60 gave 2.58× with a worst-case PSNR of
39.4 dB and a worst-case histogram similarity of 0.989. The floors are encoder-sanity gates sitting well
below anything the corpus contains — if a real frame ever fails one, the answer is to raise `--quality`,
not to lower the gate. A synthetic test fixture is not evidence about them: high-frequency noise measures
23.6 dB at the same setting, which is why `writeTestJPEG` in `internal/macosnative` is shaped like a
desktop.

## What this package assumes about the rest of the system

- **Two filenames differing only by extension belong to the same event.** Both the reconcile sibling rule
  and the passes' willingness to overwrite an existing destination rest on it. It holds because capture
  names files uniquely per display per instant and per chunk per track, and because a duplicate frame
  inserts no row at all — so a same-stem/different-extension pair across two events cannot arise. A coarser
  naming scheme would silently re-arm both hazards, which is why `findConflicts` exists as a backstop and
  why this assumption is written down rather than left in the filenames.
- **Concurrent `lumi transcript backfill` and `lumi prune` are accepted races, and neither destroys data.**
  A backfill that resolves a path and then opens it can find the file replaced underneath; it degrades to
  the text path or to a verdict with no envelope, which is pre-existing behaviour for aged-out media rather
  than a new failure — but it *is* silent, so `--older-than`'s 48-hour default matters as more than
  politeness. A prune that deletes a row this run has just repointed unlinks the old path and leaves the
  new file unreferenced, where reconcile can never reach it because its sibling is no longer named by any
  row: a leaked file, not a lost one. The recorder is gated in `internal/cli` because it is the writer with
  the highest contention, not because the others are safe by analysis.
