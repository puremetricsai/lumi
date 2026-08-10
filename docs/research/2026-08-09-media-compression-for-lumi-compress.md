# Compressing a Lumi index — research for `lumi compress`

**Date:** 2026-08-09
**Status:** research only. Nothing here is implemented.
**Review:** reviewed against the codebase by Codex on 2026-08-09; its corrections are folded in — the
`internal/wav` boundary (§3), the `FrameComparer` non-issue and the literal reading of the prune-only
deletion rule (§5), the durability/race/verification gaps in the replacement sequence (§5), and Phase 2
needing a migration rather than a path fragment (§2).

Every ratio below was measured on the author's live index (`~/Library/Application Support/Lumi`,
2026-08-02 → 2026-08-04, 7 013 events), not taken from a codec's marketing page. Measurement used
`sips`, `afconvert`, and `ffmpeg` as convenient proxies; **the recommendations name in-process
AVFoundation / ImageIO APIs**, because `CLAUDE.md` forbids adding external binaries as runtime
dependencies and the cgo bridge already links both frameworks.

---

## 1. Where the bytes actually are

| Store | Size | Share | Files | Mean |
| --- | --- | --- | --- | --- |
| `screenshots/` | **4.67 GB** | 75 % | 5 554 | 882 KB |
| `audio/` | **1.32 GB** | 21 % | 1 486 | 930 KB |
| `lumi.db` | 152 MB | 2.4 % | 1 | — |
| `record.log` | 47 MB | 0.8 % | 1 | — |
| total | 6.19 GB | | | |

Two days of recording produced ~6 GB, i.e. **≈3 GB/day**, ≈90 GB/month. Screenshots are the problem;
everything else is rounding error against them. Effort in this doc is allocated in that proportion.

Inside the database:

| Segment | Size |
| --- | --- |
| `events_fts_data` (FTS5 index) | 49.5 MB |
| `events` | 26.3 MB |
| `audio_segments` | 2.5 MB |
| six `events_*_idx` B-trees | 3.2 MB |
| smaller FTS/segment indexes (7 segments) | 0.6 MB |
| **free pages (freelist)** | **70.3 MB — 46 % of the file** |

(Sizes are MiB throughout — `dbstat` divides by 1 048 576. The used segments sum to 82.0 MiB, which plus
the 70.3 MiB freelist is the 152.3 MiB file exactly.)

`events.text` is only 11.1 MB of real text (11.0 MB screen, 0.13 MB audio) and `metadata_json` another
8.3 MB. So the database is *already* mostly overhead and empty space, not content.

---

## 2. Screenshots — 75 % of the problem

Current encoding: full-resolution JPEG, quality 0.82, via `CGImageDestination` (`native.m:57`), one file
per display per tick, ~2 s cadence, 3456×2234 on this machine.

The decisive fact about this corpus: **consecutive frames are near-identical.** `FrameComparer` already
drops exact and near duplicates, but what survives is still a desktop where one pane changed and 95 % of
the pixels did not. A per-frame codec cannot see that; an inter-frame codec can. That single observation
is worth more than any quality-setting choice.

### Measured, 20 frames sampled across the corpus (per-frame codecs)

| Variant | Size | Ratio | Notes |
| --- | --- | --- | --- |
| JPEG q82 (today) | 16.75 MB | 1.0× | baseline |
| JPEG q50 | 10.72 MB | 1.6× | |
| JPEG q35 | 7.72 MB | 2.2× | |
| **HEIC q80** | 8.33 MB | **2.0×** | full resolution, in-process via ImageIO |
| **HEIC q60** | 6.43 MB | **2.6×** | full resolution |
| HEIC q40 | 4.36 MB | 3.8× | full resolution |
| ½-resolution JPEG q60 | 4.56 MB | 3.7× | |
| ½-resolution HEIC q70 | 2.97 MB | 5.6× | **visibly degraded** — see below |

### Measured, 120 *consecutive* frames (inter-frame codecs)

| Variant | Size | Ratio | Per frame |
| --- | --- | --- | --- |
| JPEG q82 (today) | 105.8 MB | 1.0× | 903 KB |
| libx265 CRF 28 | 19.8 MB | 5.3× | 169 KB |
| libx265 CRF 34 | 12.8 MB | 8.3× | 109 KB |
| **HEVC VideoToolbox q55** | 11.0 MB | **9.6×** | 94 KB |
| **HEVC VideoToolbox q40** | 6.5 MB | **16.3×** | 56 KB |

These compare *size at a chosen encoder setting*, not size at matched visual quality; the legibility check
below is what stands in for the latter. The samples are the author's own capture and are deliberately not
checked into the repository, so the numbers are reproducible only against a live index.

Hardware HEVC produced substantially smaller files than x265 at the settings tried, and it is what
AVFoundation would use in-process anyway. That is **not** a rate-distortion claim: the quality settings
are not comparable across the two encoders and no equal-quality comparison was run, so this says the
VideoToolbox path is good enough and convenient, not that it is objectively better than x265.

### Legibility, checked by eye on a 1200×500 crop of a text-heavy frame

- **HEVC q55 — indistinguishable from the original.** Sidebar channel names, 11px metadata rows, and
  Arabic script all render identically to the JPEG.
- **HEVC q40 — still fully readable.** Only the tiny burned-in overlay badges inside video thumbnails
  soften. No interface text was harmed.
- **½-resolution HEIC q70 — noticeably degraded.** Small UI text survives but is mushy; Arabic script
  loses its diacritics. This is the one option that trades away information you might want back.

**Downscaling is the trap.** It looks like the cheapest 5× available, and it is the only one that
destroys the option to re-OCR later with a better model. The OCR text is already in FTS5, so the image is
kept for *human* review — but "human review" includes zooming into a config value that Vision misread.
Full-resolution HEVC gives a *better* ratio than half-resolution HEIC (9.6× vs 5.6×) with no visible loss.
There is no reason to downscale.

### The cost of the video path

Encoding a run of frames into one `.mov` breaks the one-file-per-event model, and the cost is larger than
an earlier draft of this doc claimed. `events.media_path` is a plain TEXT column, so it can physically
hold `…/2026-08-03T00.mov#42` — but "fits in the column" is not "needs no schema change":

- **Retention breaks outright.** `internal/retention` calls `os.Stat` and `os.Remove` on `media_path`.
  Neither understands a fragment, so pruning a compressed event either errors or silently no-ops.
- **Many events now share one file, so deletion needs reference counting.** Pruning one frame of a
  120-frame movie must not delete the movie, and the last frame's removal must. That is lifetime state
  the current model has nowhere to put.
- **`media_path` is public.** `lumi search --json` and the MCP tools hand it to callers as an ordinary
  filesystem path. A pseudo-path silently breaks every consumer that does the obvious thing with it, so
  Phase 2 also owes a real retrieval API (`lumi frame <event-id>` or equivalent) rather than expecting
  callers to parse a fragment.

So Phase 2 is a **schema change**, and per `internal/store/CLAUDE.md` it goes through
`migrations.go` as a new additive migration — most likely a nullable `media_frame` column plus a container
table, not a fragment glued onto a path. Retrieval also costs an `AVAssetImageGenerator` seek instead of
an `open(2)`. That is a real cost, and it buys a 4.7 GB → ~490 MB reduction.

The honest intermediate is **HEIC per frame at 2.0–2.6×**: one file per event, no consumer changes, no new
concepts, roughly 4.7 GB → 1.8 GB. It is the right first release; the video path is the right second one.

---

## 3. Audio — 21 % of the problem, and the most surprising result

Current encoding: 16 kHz mono 16-bit LinearPCM WAV (`native.m:1168`), 30 s chunks, two tracks per chunk
(system + microphone), 960 KB per file **regardless of content**.

That last clause is the whole story. Measured over the corpus, **325 of 727 chunks (45 %) are marked
`origin = 'silent'`**, and a silent 16-bit PCM track is 960 KB of zeroes on disk.

### Measured, 12 sampled WAVs

| Codec | Total | Ratio | Container |
| --- | --- | --- | --- |
| WAV PCM (today) | 11.1 MB | 1.0× | |
| **FLAC** | 1.53 MB | **7.2×** | `.flac`, lossless |
| ALAC | 1.59 MB | 7.0× | `.m4a`, lossless |
| AAC 32 kbps | 0.77 MB | 14.4× | `.m4a` |
| Opus 24 kbps | 0.51 MB | 21.8× | `.caf` (AudioToolbox does encode Opus — verified) |
| AAC 16 kbps | 0.42 MB | 26.2× | `.m4a` |
| Opus 12 kbps | 0.34 MB | 32.9× | `.caf` |

### Per-file, which is where it gets interesting

| Track | WAV | FLAC | AAC 32k | Opus 24k |
| --- | --- | --- | --- | --- |
| system (six samples, all silent) | 944 KB | **4 KB** | 8 KB | 16 KB |
| microphone (six samples, speech) | 944 KB | 184–440 KB | 120–124 KB | 56–88 KB |

**Lossless FLAC compresses a silent system track 236×.** Every silent chunk collapses to a 4 KB file that
is still a real, decodable audio file — which is the outcome you want, because deleting it would violate
"never lose captured media" and would make a silent chunk indistinguishable from one never recorded (the
same distinction `origin = 'silent'` exists to preserve).

So for audio the recommendation is **FLAC, lossless, unconditionally**: 1.32 GB → ~185 MB, with no
fidelity question to argue about, no re-transcription risk (measured below), and no threshold to tune.
Lossy codecs buy another 2–3× on top and are not worth the argument at this scale.

### The gate that makes or breaks this

Three code paths read the WAVs after capture, and they do **not** agree on what they can open:

| Reader | Accepts |
| --- | --- |
| `speech.swift:139` — `AVAudioFile(forReading:)` | anything AVFoundation decodes; FLAC confirmed by test below |
| `lumi transcribe <file.wav>` → same bridge | same |
| **`wav.ReadEnvelope`** (`internal/wav`) | **mono 16-bit PCM RIFF only** (tag `0x0001`, or `0xFFFE` with the PCM GUID) |

**The first row is measured, not assumed.** No test in this repository feeds the bridge anything but a
WAV, so it was checked directly: one microphone chunk carrying speech was encoded to FLAC and both files
run through `lumi transcribe`. **The two transcripts were character-for-character identical.** So
SpeechAnalyzer really does re-transcribe compressed audio through the existing bridge, with no change to
`speech.swift`. (Per the root `CLAUDE.md` rule, the transcript text stays out of this document; only the
result that they matched is recorded.) A regression test for this belongs with the implementation, and it
must read its sample from the environment and skip without one.

`wav.ReadEnvelope` is the blocker, and it is not a marginal path: `capture.measureInternalEnergy`
(`recorder.go:709`) and the backfill's `measureBackfillEnergy` (`transcript_backfill.go:422`) both call it
to decide bleed. Transcode a WAV that a future backfill still needs an envelope from and the read fails —
and `measureBackfillEnergy` swallows that error with a bare `return` (line 427), leaving the chunk with no
envelope and the verdict decided without it. The recorder's copy logs at debug and continues. So the
failure is quieter than an aged-out history: it produces a *changed bleed verdict with no diagnostic
anywhere*, which is exactly the failure class `CLAUDE.md` is written to prevent.

Two ways out:

1. **Decode compressed audio in `internal/macosnative`, and keep `internal/wav` measuring samples.**
   `internal/wav` is deliberately pure Go with no cgo — its `CLAUDE.md` scopes it to "the mono 16-bit PCM
   WAVs Lumi captures", and pulling AVFoundation into it would make a package that currently builds and
   tests anywhere depend on a Mac. The split that respects the existing boundary is: `macosnative` decodes
   any AVFoundation-readable file to mono 16-bit PCM samples, `internal/wav` keeps `Envelope` over those
   samples unchanged. `ReadEnvelope` (the file-reading half) is what grows a second implementation.
2. **Only compress chunks nothing will re-read**: segments present, not in `ChunksFailedTranscription`.
   Cheaper to build, but it permanently strands the failed chunks at 960 KB each — and those are precisely
   the ones a user is most likely to keep for a manual retry.

---

## 4. The database — small, but the cheapest win in the entire document

```
152.3 MB  →  VACUUM  →  86.8 MB      in 0.76 s
```

46 % of the file is freelist pages left by earlier `lumi prune` runs. SQLite never returns them to the
filesystem without a `VACUUM`, and `auto_vacuum` cannot be enabled on an existing database without a
full rebuild anyway.

This is one statement, it is lossless, it is fast, and `lumi prune` — the command that *creates* the free
pages — does not do it today. `lumi compress` should run it every time.

Caveats worth stating in the command's help text: `VACUUM` needs up to 2× the file size in scratch space,
cannot run inside a transaction, and needs an exclusive write lock. Under WAL with the store's busy
timeout, a merely *open* connection does not block it — an active transaction or write-lock holder does,
so a recording `lumi record` will, and an idle `lumi mcp` generally will not. Report a blocked vacuum as a
skipped step, not an error.

Note the reclaim is 65.5 MiB against a 70.3 MiB freelist. `VACUUM` does not drop free pages in place — it
copies every live table and index into a fresh file and swaps it over the original, which also rewrites
each B-tree in key order. That repacking changes fill factors, so "file minus freelist" predicts the
outcome only approximately, and the defragmentation is a second, unbilled benefit of running it.

**The ROWID caveat, which is free today and a trap for Phase 2.** `VACUUM` may renumber the ROWIDs of any
table that does not declare an `INTEGER PRIMARY KEY`, silently invalidating every reference held outside
that table. Both current tables are safe: `events.id` and `audio_segments.id` are declared
`INTEGER PRIMARY KEY`, which makes them aliases for the rowid rather than separate columns, so their
values survive a vacuum and `get_event`, `EventByID` and `audio_segments.event_id` all keep resolving.

This stops being free the moment Phase 2 lands. The container table §2 calls for would be referenced by
`events` rows, and `lumi compress` is proposed to run `VACUUM` **in the same invocation** that writes those
references — so a container table declared without `INTEGER PRIMARY KEY` would have its rows renumbered by
the very command that just pointed at them, turning every compressed event toward the wrong movie with no
error anywhere. Any table a future migration adds must declare `INTEGER PRIMARY KEY` (or be keyed on
something other than the rowid), and that requirement belongs in `internal/store/CLAUDE.md` next to the
existing migration rules, not only here.

**FTS5 retuning is deliberately not recommended.** `events_fts_data` is 49.5 MB indexing 11.1 MB of text —
a 4.5× ratio that looks wasteful until you notice it is 0.8 % of the 6 GB problem. Switching to
`detail=column` or `detail=none` would shrink it, but `detail=none` rejects phrase queries, and
`ftsExpression` (`query.go`) quotes every term — a quoted term that tokenizes to more than one token *is*
a phrase query. The change is a search-correctness risk for 30 MB. Not worth it.

`record.log` at 47 MB has no rotation at all. That is a bug to fix in the daemon, not work for
`lumi compress`.

---

## 5. Recommended shape for `lumi compress`

Mirror `lumi prune`, which users already know:

```
lumi compress [--older-than 48h] [--screens heic|none] [--audio flac|none]
              [--vacuum] [--sweep-orphans] [--dry-run] [--json]
```

`--screens hevc` is deliberately absent from this signature: per §2 it needs a migration, a
frame-retrieval API, and reference-counted movie lifetimes, so it is a Phase 2 value added to an existing
flag rather than something Phase 1 ships behind a flag it cannot honour.

**Age-tiered, defaulting to leaving recent capture alone.** Recompression is what you do to history, not
to the frame captured four seconds ago; a default like `--older-than 48h` keeps the backfill working on
untouched originals.

**Idempotent by file extension.** A row whose `media_path` ends in `.heic` or `.flac` is already done.
For Phase 1 this needs no schema change, no state column and no migration — matching the existing
preference for reusing what is there over adding fields. (Phase 2 does need a migration; see §2. The
extension trick scales to one-file-per-event only.) This covers *re-running* compress; it does **not**
recover crash leftovers (see below), which need a separate sweep.

**Replacement order is the inverse of prune's, on purpose.** Prune deletes rows before files, because an
orphaned file is recoverable and a row pointing at nothing is not. Compress must do the opposite:

> encode → verify → fsync file **and its directory** → `UPDATE events SET media_path` → delete the original

Crash anywhere in that sequence leaves either two copies of the media or an orphan, never a row pointing
at a file that does not exist. Both are recoverable; the third is not. Without the directory fsync the
rename/create may not survive a power loss that the committed row update does, which reintroduces the case
the ordering exists to prevent. **State this inversion in `internal/retention/CLAUDE.md` when
implementing** — otherwise the next reader assumes the rows-first rule is universal and "fixes" it.

**Crash leftovers need their own sweep, and extension-idempotence does not provide it.** A crash before
the `UPDATE` leaves an orphan compressed file; a crash after it leaves an orphan original. Neither is
visible to a rerun that only asks "does this row's path end in `.heic`". `prune --all` already sweeps the
media directories for files no row references, which is the right mechanism — it just has to be reachable
without wiping the index. A `lumi compress --sweep-orphans` (or making the existing orphan sweep a
standalone flag) closes this; leaving it out means every crash permanently leaks a file.

**Verify by comparing decoded content, not just by opening the file.** A truncated or corrupt encode can
still decode and still report the expected duration and dimensions. For audio, lossless FLAC makes this
exact and cheap: decode the result and compare sample data (or a hash of it) against the source — a
guarantee no lossy path can offer. For images, compare decoded dimensions plus a perceptual distance
against the original rather than trusting the header. A silent encoder failure that then deletes the
source is the one bug in this feature that destroys data.

**The `UPDATE` must be conditional, because compress races the other readers.** Prune and the backfill
both read `media_path` and open the file afterwards; a compress running concurrently can invalidate the
path between those two steps. Write `UPDATE events SET media_path = ? WHERE id = ? AND media_path = ?` so
a row another writer already changed is skipped rather than clobbered, and treat a zero-row result as
"someone else got there first", not as an error. This narrows but does not close the window — a
single-writer story (compress refuses to run while `lumi record` is active, as `VACUUM` already forces)
is the honest resolution.

### Invariants this feature collides with — flagged, not resolved

- **`internal/retention/CLAUDE.md`: "`lumi prune` is the only path permitted to delete media."** Read
  strictly, this is already not literally true — the recorder unlinks duplicate frames before indexing
  them (`recorder.go:365`) and the native audio writer removes an empty or pre-existing destination
  (`native.m:1158`, `native.m:1237`). What the rule actually protects is media *an event row points at*,
  and there `lumi compress` really would be a second owner, with the opposite crash ordering. So this is
  not a one-line amendment: prune and compress have to be described together, as two deletion paths whose
  orderings are each correct for what they are protecting. Do not implement around it silently.
- **Root `CLAUDE.md`: "Never lose captured media."** Lossless FLAC satisfies this outright — the samples
  are recoverable bit-for-bit. **HEIC and HEVC do not**, and this doc should not pretend the question is
  settled by "the event and its file survive": a lossy re-encode discards pixel data permanently, and the
  invariant was written about a capture pipeline that had no lossy step after the initial JPEG. Shipping
  the image path needs an explicit, recorded decision that a second lossy generation is acceptable — not
  an inference from wording that predates the question. The strongest argument for yes is that the OCR
  text is already extracted and the visual check found no legible difference at q55; the strongest
  argument for no is that it is unrepeatable and the user cannot preview what they are giving up.
- **`FrameComparer` is *not* affected, contrary to an earlier draft of this doc.** It imports `image/jpeg`
  and decodes with `image.Decode` (`compare.go:3`, `:127`), but its per-display state is an in-memory
  hash plus histogram (`frameState`), never a stored path, and `Duplicate` runs on the frame just written,
  before the event is inserted (`recorder.go:359`). Compress walks indexed rows, so it cannot reach a
  frame that has not been compared yet — even at `--older-than 0s`. No bound on the flag is needed for
  this reason. (One is still needed so compress does not fight a live recorder for the write lock.)
- **`internal/wav` must be widened before any audio transcode.** Section 3. This is the item that makes
  the recommendation wrong if skipped.
- **`UPDATE events SET media_path` fires the `events_au` trigger, once per compressed row.** The trigger
  names `text`/`app`/`window` unconditionally, so every rewrite deletes and reinserts the row's FTS entry
  even though none of those columns changed. It re-syncs to the same values, so this is churn rather than
  a correctness problem — but `internal/store/CLAUDE.md` cites exactly this cost as a reason the repo
  refuses to rewrite existing rows, so it should be an accepted cost, not an unnoticed one. It is also the
  second reason `VACUUM` belongs at the *end* of a compress run: the churn is a large part of what there
  will be to reclaim.

### Expected outcome on this corpus

| Stage | Now | Phase 1 (HEIC + FLAC + VACUUM) | Phase 2 (HEVC + FLAC + VACUUM) |
| --- | --- | --- | --- |
| Screenshots | 4.67 GB | 1.80 GB | 0.49 GB |
| Audio | 1.32 GB | 0.18 GB | 0.18 GB |
| Database | 0.15 GB | 0.09 GB | 0.09 GB |
| **Total (media + db)** | **6.14 GB** | **2.07 GB (3.0×)** | **0.76 GB (8.1×)** |

Totals exclude the 47 MB `record.log`, which is why they do not match the 6.19 GB in §1.

Phase 1 needs no new concepts and no consumer changes. Phase 2 needs a migration, a frame-retrieval API,
and reference-counted movie lifetimes, and pays for all of that with another 2.7×.

---

## 6. What this research deliberately does not propose

- **Deleting anything.** That is `lumi prune`'s job and it already exists. Compress and prune are
  complementary: prune decides what history to keep, compress decides how densely to keep it.
- **Downscaling screenshots.** Measured worse than full-resolution HEVC *and* it forecloses re-OCR.
- **Lossy audio.** Lossless FLAC already yields 7.2× corpus-wide and 236× on silent tracks; the remaining
  2–3× is not worth a fidelity argument on the only recording of a conversation.
- **FTS5 `detail=` retuning.** 30 MB of savings against a search-correctness risk, inside a 6 GB problem.
- **Summarizing or discarding OCR text to shrink the index.** `events.text` is 11.1 MB. It is also the
  entire point of the product.
- **A background compression scheduler.** `internal/retention` has no scheduler by design; compress should
  match that and stay a command the user runs.
- **Deduplicating identical screenshots across time.** `FrameComparer` already handles this at capture,
  and inter-frame HEVC subsumes what is left.
- **Content-defined chunking / whole-directory `zstd`.** Both beat per-file codecs on paper and both make
  the media unreadable by AVFoundation, `open`, and the user. The value of Lumi's media is that it is
  ordinary files.

---

## Appendix — how to reproduce

Sampling and encoding were run over the live index; artifacts are in this session's scratchpad, not the
repository (per `CLAUDE.md`: real captured conversation never becomes a fixture — only the numbers above
are recorded here).

```sh
# database
sqlite3 lumi.db "SELECT name, sum(pgsize)/1048576.0 mb FROM dbstat GROUP BY name ORDER BY mb DESC;"
sqlite3 lumi.db "PRAGMA freelist_count; PRAGMA page_count;"

# images, per frame
sips -s format heic -s formatOptions 60 in.jpg --out out.heic

# images, inter-frame (1 fps concat over consecutive captures)
ffmpeg -f concat -safe 0 -r 1 -i list.txt -c:v hevc_videotoolbox -q:v 55 -tag:v hvc1 out.mp4

# audio
afconvert -f flac -d flac in.wav out.flac
afconvert -f caff -d opus -b 24000 in.wav out.caf

# the gate check of §3: these two must print identical text
./lumi transcribe chunk.wav
./lumi transcribe chunk.flac
```

Legibility was judged on identically-positioned 1200×500 crops extracted with
`ffmpeg -i … -vf crop=1200:500:400:600`, taking the video variants' first frame via
`-vf "select=eq(n\,0)"` so every variant shows the same source frame and the same region.

**Two numbers in here are worth re-measuring before acting on them**, because both depend on this
machine's usage rather than on Lumi: the 45 % silent-chunk share (a user on calls all day will see far
less silence, and audio's 7.2× shrinks toward FLAC's ~2× speech ratio) and the 46 % freelist (an index
that has never been pruned has none, and `VACUUM` will reclaim nothing). The screenshot numbers are the
robust ones — they depend on desktop capture being mostly-static, which is intrinsic to the product.
