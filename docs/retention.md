# Retention

Captured JPEG and WAV files remain on disk until explicitly pruned. Preview an age policy before applying it:

```sh
./lumi prune --older-than 720h --dry-run
./lumi prune --older-than 720h
```

You can instead cap indexed media by bytes, or combine both policies. Age pruning runs first; the size pass then removes the oldest remaining events until the cap is met.

```sh
# Keep indexed media under 50 GiB
./lumi prune --max-bytes 53687091200

# Combine age and size policies and emit a machine-readable preview
./lumi prune --older-than 2160h --max-bytes 53687091200 --dry-run --json
```

To wipe everything, `--all` deletes every indexed event and all media, then sweeps the `screenshots/` and `audio/` directories for orphaned files no row referenced. It is irreversible, so it prompts you to type `yes`; only `--yes` (for scripts) or `--dry-run` (which deletes nothing) skips the prompt.

```sh
./lumi prune --all --dry-run
./lumi prune --all
```

Lumi does not schedule retention automatically. Run `prune` periodically yourself if you want a fixed policy. Database rows are deleted before their media files, so an interrupted prune can leave recoverable orphaned files rather than indexed events whose media has disappeared.
