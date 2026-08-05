# Custom vocabulary

Apple's on-device transcriber sometimes mishears names and jargon outside its general vocabulary, and a
misheard term is permanently unsearchable. Drop an optional `vocabulary.txt` in your data directory — one
term or phrase per line, UTF-8 — to bias recognition toward it:

```text
# people
Mostafa
Lumi

# jargon
SpeechAnalyzer
```

Blank lines and lines starting with `#` are ignored, surrounding whitespace is trimmed, and exact duplicate
terms collapse to their first occurrence. File order is priority order: only the first 100 terms are used,
and anything past that cap is dropped rather than silently ignored — `lumi doctor` reports how many. An edit
takes effect on the next audio chunk; no restart needed.

Compare the effect on fixed audio with `lumi transcribe`, which replays one WAV through the same
transcription path the recorder uses:

```sh
./lumi transcribe recording.wav --no-vocabulary   # baseline
./lumi transcribe recording.wav                    # with vocabulary.txt applied
./lumi transcribe recording.wav --vocabulary other.txt  # a specific list instead
```

Comparing two live recordings would confound the vocabulary with how the words happened to be spoken;
replaying the same file isolates the term list as the only variable. `lumi transcribe` also takes
`--speech-locale` (same default as `record`, `en-US`), for replaying audio in a non-default locale.

An explicit `--vocabulary <path>` that is missing or unreadable is a hard, non-zero error — the behavior
you're most likely to hit by accident, for example a typo'd path or an unset `--vocabulary="$VOCAB"`. This
is deliberate: silently falling back would print an ordinary baseline transcript that looks
vocabulary-assisted, defeating the comparison this command exists to make. The default file (no
`--vocabulary` given) is different — its absence stays silent, since running with no vocabulary at all is a
legitimate baseline.
