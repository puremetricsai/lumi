# Compression

`lumi compress` re-encodes the media already on disk into smaller files, leaving every event exactly where
it is. Screenshots become HEIC, audio becomes lossless FLAC, and the database is rebuilt to return the free
pages a prune left behind.

Compress and prune are complementary: prune decides what history to keep, compress decides how densely to
keep it. Nothing compress does deletes an event.

Preview before applying, the same way you would a prune:

```sh
./lumi compress --dry-run
./lumi compress
```

A dry run reports how many files would be compressed and how much they weigh now, but **cannot report a
ratio** — measuring one means doing the encode.

## What it does, and roughly what it saves

Measured on a real 6.2 GB index of two days' capture:

| | before | after |
| --- | --- | --- |
| `screenshots/` | 4.67 GB | ~1.8 GB |
| `audio/` | 1.32 GB | ~0.18 GB |
| `lumi.db` | 152 MB | ~87 MB |

Screenshots are three quarters of the problem and audio most of the rest. The database shrinks because
roughly half of it is free pages left behind by earlier prunes, which SQLite never returns to the
filesystem without a `VACUUM`.

By default it only touches events older than 48 hours:

```sh
./lumi compress --older-than 720h     # only compress the last month's history and older
./lumi compress --older-than 2026-08-01T00:00:00Z
```

`--older-than` takes a Go duration or an RFC3339 timestamp, exactly like `prune`'s. Go durations have no
`d` unit, so use `720h` rather than `30d`. The default exists because recompressing what was captured a
moment ago competes with the recorder for files it is still writing, and takes away the untouched
originals a later `lumi transcript backfill` may want.

## Screenshots are re-encoded lossily. This is the decision to read before running it.

Lumi captures screenshots as JPEG. HEIC re-encoding is therefore a **second lossy generation**, and it
cannot be undone: the discarded pixels are gone, and there is no way to preview what you are giving up
before you give it up.

It is on by default anyway, and here is the reasoning both ways.

**For:** the OCR text was extracted at capture and is already in the search index, so the image is kept for
*human* review rather than for re-reading by machine. A visual check at the shipped quality found no
legible difference — sidebar labels, 11px metadata rows and Arabic script all render as they did in the
original. Measured across 309 frames sampled from a real index, the worst frame scored 39.4 dB PSNR, which
is a long way from visible damage. It buys 2.58×, on the largest part of the problem.

**Against:** "human review" includes zooming into a configuration value that Vision misread, and that is
exactly the case a lossy re-encode erodes. It is unrepeatable, and you cannot see the cost in advance.

**To decline it**, compress audio and the database only:

```sh
./lumi compress --screens none
```

Audio raises no such question. FLAC is lossless — the samples come back bit for bit — so the only thing at
stake is disk space. It was also checked directly: a real captured chunk transcribed to character-identical
text before and after encoding, so re-transcribing a compressed chunk reaches the same words.

You can also trade differently rather than declining:

```sh
./lumi compress --quality 0.8      # larger files, less loss
./lumi compress --quality 0.4      # smaller files; still legible in the measured sample
```

## Nothing is deleted before its replacement is verified

A silent encoder failure that then deletes the source is the one thing in this command that would destroy
data, so the encoder's own report of success is never trusted. Each re-encoded file is reopened and
decoded, and only then does the original go.

- **Audio is verified exactly.** The result is decoded and compared against the source sample for sample.
  FLAC being lossless, this is a real guarantee rather than a threshold.
- **Images are verified against three independent checks**: the decoded dimensions must match the source,
  the PSNR must clear `--min-psnr` (30 dB by default), and a colour histogram comparison must match. They
  are independent because a colour-space or channel-order mistake can pass one and fail another.

If a file fails, the command says so, keeps the original, keeps the row, and moves on:

```
2 screenshots failed verification and were left untouched; their originals are intact
```

That line means an encoder is misbehaving. Nothing was lost, and re-running is safe.

## Interrupting it is safe

Compress writes the replacement, flushes it, repoints the row, and only then deletes the original — the
opposite order from `prune`, which deletes rows before files. Each ordering is right for what it protects:
an orphaned file is recoverable, and a row naming media that does not exist is not.

So a crash, a `Ctrl-C`, or a power loss leaves either two copies of a file or one unreferenced file, never
a row pointing at nothing. The next run reconciles whatever was left behind, and says so:

```
cleaned up 2 leftover files from an interrupted run, 1.7 MiB
recovered 41 events whose media an interrupted run had left unreferenced
```

`recovered` means an earlier run was interrupted after writing a compressed file but before its row update
reached disk — the row's original was already gone, and the surviving compressed file was adopted rather
than deleted. Both lines are informational; nothing is wrong.

Re-running is always safe. A row whose media is already HEIC or FLAC is simply counted as done.

Only one compress may run at a time. A second refuses:

```
compression is already in progress; two runs would race for the same files
```

The lock is held by the running process rather than recorded in a file, so a run killed outright leaves
nothing to clean up and the next run simply proceeds.

It also refuses while `lumi record` is running, since it rewrites the media paths the recorder is using.
Stop the recorder, or pass `--while-recording` to override.

## Copying your index: compress refuses rather than reaching outside

`events.media_path` is stored as an absolute path, so a copied database still names the files in the
*original* directory. `--data-dir` redirects which database is opened, not where the media it points at
lives. Every other command only reads media, so this has never mattered; compress is the first one that
rewrites and deletes it — which without a guard means running against a copy destroys the original's files.

So it refuses:

```
error: 7040 indexed events name media outside this data directory (for example …); compress rewrites and
deletes the files it reads, so it will not touch media belonging to another index.
```

The whole run fails rather than compressing the subset that happens to be inside, because media outside
the data directory means you are not looking at the index you think you are.

To experiment on a copy, rewrite the paths so they point inside it:

```sh
cp -R ~/Library/Application\ Support/Lumi /tmp/lumi-copy
sqlite3 /tmp/lumi-copy/lumi.db \
  "UPDATE events SET media_path = replace(media_path, '$HOME/Library/Application Support/Lumi', '/tmp/lumi-copy');"
./lumi --data-dir /tmp/lumi-copy compress --dry-run
```

If you have deliberately moved your media elsewhere, move the data directory as a whole and rewrite the
paths the same way; compress will only ever touch files beneath the `screenshots/` and `audio/` directories
of the index it was given.

## The database rebuild

`VACUUM` runs at the end of every invocation, which is where most of the database saving comes from. It
runs last for two reasons: it reclaims the index churn the compression passes themselves created, and it
needs an exclusive lock that nothing else in the run should be competing for.

It needs up to twice the database file's size in free space while it works. If the database is busy the
step is skipped rather than failed — everything already compressed stays compressed:

```
vacuum: skipped, the database is in use
```

Turn it off with `--vacuum=false`.

## How long it takes

Files are re-encoded several at a time — one per core, up to 8 — which is about 3.8x faster than doing them
one after another. Measured over 180 real 3456×2234 frames on an 18-core machine: 37.6 s sequentially,
9.9 s at 8 workers. Beyond 8 the gain stops (9.3 s at 18) while memory keeps climbing, which is why that is
the ceiling.

Budget roughly 55 ms per screenshot at the default concurrency, most of it the verification decode rather
than the encode. A 5,500-frame index takes about 5 minutes; audio is far faster. It is safe to interrupt and
resume.

`--workers 1` goes back to one file at a time, and `--workers N` sets it explicitly. Peak memory is about
90 MB per file in flight on top of a ~1.4 GB baseline, so lower it if you are short on RAM.

Each pass reports where it is on stderr every few seconds, so a long run is visibly working rather than
apparently hung:

```
level=INFO msg="compressing screenshots" done=109 of=223 elapsed=20s
level=INFO msg="finished compressing screenshots" done=223 of=223 elapsed=42s
level=INFO msg="rebuilding the database to reclaim free pages" database=… bytes=111026176
```

`of` counts the files that pass will actually replace — the same number `--dry-run` reports — not the rows
it looked at to find them, most of which are aged-out or already compressed. A run short enough to finish
inside one interval prints none of this.

Prefer to know the size of the job first? `--dry-run` reports it in about a second and writes nothing.

Screenshots are compressed before audio, and interrupting stops the whole run — so a `Ctrl-C` during a slow
screen pass gives up the audio pass too, which is the lossless one and the better ratio (around 7x against
2.6x). If you only have a few minutes, `--screens none` does the cheap half.

## Machine-readable output

```sh
./lumi compress --json
```

Reports per-pass counts (`files`, `bytes_before`, `bytes_after`, `already_done`, `skipped`,
`missing_files`, `encode_failed`, `verify_failed`, `flush_failed`, `raced`, `conflicted`), the reconcile
counts, and the vacuum's status.

`conflicted` counts rows left alone because compressing them would have taken another row's media with
them — two events naming one file, or an event whose compressed name another event already holds. Neither
happens in an index Lumi wrote; a non-zero count means the index has been edited or merged.

## What it deliberately does not do

- **Delete anything indexed.** That is `lumi prune`.
- **Sweep unreferenced files in general.** Captured media exists on disk before its event row does, so a
  general sweep would delete frames the recorder had not indexed yet. Compress only reconciles a file whose
  extension-swapped sibling is named by a row. The general sweep remains `prune --all`'s, behind its
  confirmation prompt.
- **Downscale screenshots.** It measures worse than full-resolution encoding and it forecloses re-running
  OCR later with a better model.
- **Compress audio lossily.** Lossless already collapses a silent track by several hundred times, and the
  remainder is not worth a fidelity argument about the only recording of a conversation.
- **Run on a schedule.** Like `prune`, it is a command you run.
