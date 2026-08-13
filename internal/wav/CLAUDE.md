# internal/wav

Reads the mono 16-bit PCM WAVs Lumi captures and measures their energy. Exists because the recognizer
returns nothing both for a silent track and for one carrying audio it could not transcribe, and those need
opposite conclusions.

- **`ReadMono16` walks RIFF chunks generically** and accepts fmt tag `0xFFFE` with a PCM SubFormat GUID,
  because Lumi's own writer emits `RIFF/WAVE` → `JUNK` → `fmt ` → `FLLR` → `data` — a reader assuming
  `fmt ` at offset 12 fails on every file Lumi has recorded.
- **Energy is measured as an envelope, never a whole-file RMS**, and it answers presence rather than
  loudness. The rule and the measurements behind it are `internal/transcript/CLAUDE.md`
  (`EnvelopeWindowMS`, `NeedsInternalEnergy`); this package only supplies the samples.
- **This package reads Lumi's *WAVs*; anything else goes through `capture.ReadAudioEnvelope`**, which
  decodes natively and hands the samples back to `Envelope` here. Keeping cgo out is the point: a package
  that builds and tests anywhere must not gain a dependency on a Mac because `lumi compress` learned to
  store FLAC.
