# Native capture verification baseline

**Date:** 2026-07-19
**Host:** Apple M5 Max, macOS 26.5.2, arm64
**Go:** 1.24+

## Automated verification

```sh
go test ./...
go vet ./...
task bench:capture
```

The initial sampled-histogram benchmark over a 1920×1080 JPEG reported:

```text
BenchmarkSampledHistogram-18  207  5700970 ns/op  5.83 MB/s  3201749 B/op  14408 allocs/op
BenchmarkFrameComparerExactDuplicate-18  54411  21914 ns/op  41384 B/op  5 allocs/op
```

The histogram fallback is approximately 5.7 ms per changed frame on the baseline host, while an exact static frame takes about 22 µs because the hash fast path skips JPEG decoding. Both are well below Lumi's default two-second capture interval. Recorder tests additionally prove that repeated per-display frames are deleted, Accessibility text bypasses Vision, Vision is used when Accessibility text is empty, system/microphone provenance survives the SQLite round trip, and the ten-second maximum-silence rule periodically retains an otherwise duplicate frame.

## Permission-gated native smoke test

After granting Screen Recording, Accessibility, and Microphone access to the terminal or built Lumi binary:

```sh
task test:native
```

The stable `./lumi` binary captures every connected display, reads the focused Accessibility context, invokes Apple Vision, and records a 500 ms ScreenCaptureKit chunk. It fails unless both `system` and `microphone` WAV sources are produced. This check is intentionally opt-in because macOS TCC permission is specific to the invoking executable and cannot be granted non-interactively.

On the baseline host, the first check was gated by Screen Recording permission; `./lumi native-smoke` exited before capture with the exact System Settings location. `task permissions` opened the native request flow, after which approval under **Privacy & Security → Screen & System Audio Recording** unblocked the stable binary.

After permission was granted, `task test:native` passed with one display, a live Accessibility snapshot, Apple Vision, system audio, and microphone audio.

## Measured native capture sample

A 12-second capture used the default two-second screen interval and five-second audio chunks. Logs were redirected away from the captured display. `/usr/bin/false` stood in for whisper so the sample measured native capture and verified downstream-failure preservation without requiring a model.

| Measurement | Result |
|---|---:|
| Wall time, including final in-flight audio chunk | 15.79 s |
| User + system CPU time | 0.87 s (~5.5% of one core) |
| Maximum resident set size | 109,445,120 bytes (~104 MiB) |
| Total data directory | 6,540 KiB |
| Screenshots | 5 files, 5,312 KiB |
| Audio | 6 WAV files, 1,164 KiB |
| SQLite database | 60 KiB |
| Indexed events | 11 (5 screen, 6 audio) |

The one-display schedule allowed six screen captures; five were retained, a 16.7% reduction during an actively changing foreground session. All five used `text_source=accessibility`. Three chunks each produced separately indexed `system` and `microphone` WAV files, and `file(1)` identified all six as RIFF/WAVE audio. All six remained indexed with processor diagnostics when the fake transcriber failed. The last system/microphone pair completed after the 12-second cancellation and was also preserved, exercising the shutdown finalization path.

A separate four-second screen-only audit indexed the live frontmost app (`Ghostty`), its window title (`…/Projects/business/lumi`), 716 characters of Accessibility text, `text_source=accessibility`, and display ID 1.

## Longer resource and disk sample

For a longer representative workload, record for ten minutes and compare the indexed-event count and media size with the configured two-second interval:

```sh
./lumi record --duration 10m &
lumi_pid=$!
sample "$lumi_pid" 600 -file /tmp/lumi-sample.txt
wait "$lumi_pid"
du -sh "${LUMI_HOME:-$HOME/Library/Application Support/Lumi}"
./lumi search --since 10m --limit 500 --json
```

Record display count, retained screen-event count, system/microphone audio-event counts, data-directory growth, average CPU, and peak RSS. The retained screen-event count should be lower than the theoretical `display_count × 300` maximum on a mostly static desktop, while no display should have a gap longer than roughly ten seconds.
