# Releasing Lumi

`.github/workflows/release-please.yml` is the whole release. A push to `main` with a releasable
commit opens a release PR; merging it cuts a **draft** release, and the rest of the workflow builds,
signs, uploads, verifies, and only then publishes it.

The release carries one asset that users install, `lumi-macos-arm64.zip`, and `install.sh` at the
repository root is what installs it: it resolves `releases/latest/download/`, so nothing has to be
rewritten per release.

**The draft is what keeps a failed release away from users, and it is the only thing that does.**
GitHub's `latest` skips drafts, so it stays on the previous release for the whole build and forever
if the build fails. Published at cut instead — release-please's default — `latest` would move to an
assetless release seconds after the merge and every install would 404 until the assets landed.
`draft: true` in `release-please-config.json` and the `publish-release` job are what enforce it.

`force-tag-creation: true` sits beside it and is not optional company. GitHub does not create a git
tag for a draft until it is published, and release-please finds the previous release by its tag — so
a release that failed before `publish-release` would leave a draft the next run cannot see, and that
run would regenerate the changelog from the release before it. The option tags at cut instead. A
bare tag does not move `latest`, so it costs the draft gate nothing.

**The release publishes one archive, and the embedded binary is not one of them.** `Lumi.app` is the
whole product; the binary inside it is an implementation detail of the bundle. Nothing should
reintroduce a second published artifact, or a second install channel, without deciding again that
Lumi has two front doors.

## Repository secrets

All three are required, and the release fails without them. An ad-hoc signature has a fresh
designated requirement every build, so shipping one would cost every existing user their TCC
grants — which is the opposite of what `README.md` promises them. `build-binaries` refuses rather
than letting that through, which leaves the draft unpublished and everyone on the previous
release.

| Secret | What it holds |
| --- | --- |
| `MACOS_DEVELOPER_ID_P12_BASE64` | the signing certificate and its private key, as a base64 `.p12` |
| `MACOS_DEVELOPER_ID_P12_PASSWORD` | the password that `.p12` was exported with |
| `MACOS_DEVELOPER_IDENTITY` | the identity's common name, passed to `codesign --sign` |

The names say *Developer ID* while the certificate is self-signed on purpose. Getting a real
Developer ID later is then a change of secret *values* and no change to the workflow at all —
`.claude/commands/lumi-developer-id-signing.md` is the rest of that move.

Never commit the `.p12`, the password, or the exported certificate. The workflow prints neither.

## Why the release is signed at all, before notarization is possible

Signing and notarization need a paid Apple Developer account. Until one exists the app cannot be
notarized. `install.sh` sidesteps the consequence rather than fixing it — `curl` never writes
`com.apple.quarantine`, so Gatekeeper does not gate the first launch — but that only covers the
supported install path. A browser download of the same ZIP is quarantined and dies as measured
below.

What one stable certificate still buys, against ad-hoc signing:

| | ad-hoc (`codesign -s -`) | one self-signed certificate |
| --- | --- | --- |
| Designated requirement | the cdhash, new on every build | `certificate leaf = H"…"`, stable |
| TCC grants across an upgrade | all five lost, every release | survive |
| `install.sh` signature check | passes either way | passes either way |

The last row is the reason the workflow gates on the secret rather than leaving it to
`install.sh`: the script runs `codesign --verify --strict`, which an ad-hoc bundle passes. What it
cannot see is that the identity changed since the release before it.

So configure the secrets **before the first release anyone installs**, not after. A release that
ships ad-hoc and a later one that ships self-signed differ in designated requirement, which is a new
identity to TCC: every early user loses their permissions and re-approves, once, for nothing.

## Creating the interim self-signed certificate

Both extensions are required and the failure without them is misleading. A certificate carrying only
`extendedKeyUsage=codeSigning` is listed by `security find-identity` and then rejected by `codesign`
with `no identity found`, which reads like a keychain search-path problem and is not one.

```sh
openssl req -x509 -newkey rsa:2048 -nodes -keyout lumi-signing.key -out lumi-signing.pem \
  -days 3650 -subj "/CN=Lumi Self-Signed" \
  -addext "basicConstraints=critical,CA:false" \
  -addext "keyUsage=critical,digitalSignature" \
  -addext "extendedKeyUsage=critical,codeSigning"

openssl pkcs12 -export -out lumi-signing.p12 -inkey lumi-signing.key -in lumi-signing.pem \
  -name "Lumi Self-Signed"

base64 -i lumi-signing.p12 | pbcopy   # -> MACOS_DEVELOPER_ID_P12_BASE64
rm lumi-signing.key lumi-signing.p12  # keep lumi-signing.pem only if you want the fingerprint
```

`MACOS_DEVELOPER_IDENTITY` is then `Lumi Self-Signed`. No trust settings, no keychain trust
override, and no `sudo` are needed — measured on macOS 26.5.2, where an untrusted self-signed
identity signs with `--options runtime` and `--timestamp` and produces
`designated => identifier "com.puremetricsai.lumi" and certificate leaf = H"…"`.

Do not gate anything on `security find-identity -v`. It reports `0 valid identities found` for a
certificate `codesign` then signs with perfectly well, because "valid" there means trusted.

Rotating the certificate costs every user their TCC grants and one more Open Anyway, exactly as
moving to Developer ID will. Keep the `.p12` somewhere you will still have it in a year.

## Installing what was released

`install.sh` at the repository root is the only install channel. It gates on `arm64` and macOS 26,
downloads `releases/latest/download/lumi-macos-arm64.zip`, extracts it with `ditto` (not `unzip`,
which drops the extended attributes the signature is sealed over), verifies the signature, and moves
`Lumi.app` into `/Applications`.

`/Applications` is not a free choice. MCP registration writes the absolute in-bundle path into every
client's config, and TCC keys its grants on path and signature together, so an install elsewhere
costs users both.

It carries no version and no digest, so no release step rewrites it. What a release *does* move is
the `latest` pointer the script resolves, which is why `publish-release` runs last. The digest is the
one deliberate omission: the script is fetched over TLS from the same
origin that serves the asset, so a pinned hash would add no trust the transport does not already
carry, and `ditto` fails on a truncated archive anyway.

### What quarantine still costs

Measured on macOS 26.5.2, and the reason the install command is a `curl` pipe rather than a link to
the ZIP: a quarantined, never-executed, non-notarized Mach-O is **killed with SIGKILL and no prompt
at all** — exit 137, no output, `syspolicyd: Terminating process due to Gatekeeper rejection`.
Quarantine is written to every file inside the bundle, so it reaches the binary the app spawns as
well as the app itself.

`curl` writes only `com.apple.provenance`, never `com.apple.quarantine`, so none of that happens on
the supported path. A browser download of the same asset gets all of it, which is what `README.md`
documents the recovery for.

## Recovery

- **The build, signing, upload, or either verification job failed.** The release is still a draft,
  `latest` still names the previous one, and users are unaffected — there is nothing to roll back.
  Fix forward, or use **Re-run failed jobs** on the same run for a transient failure. Do not push a
  fresh run expecting it to republish: release-please sets `release_created` to false for a version
  whose release exists, and every job downstream of it is skipped.
- **Everything passed but `publish-release` failed.** The assets are good and verified; only the
  draft is unpublished. Re-run that job, or publish by hand:
  `gh release edit "$TAG" --draft=false --latest`.
- **A published release is missing `lumi-macos-arm64.zip`.** `upload-release-assets` fails rather
  than letting one through, so this needs a hand-deleted asset to reach. Re-upload it to the same
  tag; until then every `install.sh` run 404s.
- **The signing secrets are missing.** `build-binaries` fails at *Import the release signing
  identity* before anything is built. Set all three and re-run the job; nothing shipped, so there is
  nothing to undo. Rotating the certificate to a *different* one later is what costs every user
  their TCC grants once, and that is a decision to make deliberately.
