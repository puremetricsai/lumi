# Releasing Lumi

`.github/workflows/release-please.yml` is the whole release. A push to `main` with a releasable
commit opens a release PR; merging it cuts the tag, and the rest of the workflow builds, signs,
publishes, verifies, and then updates the Homebrew cask in this repository.

The tap serves exactly one package, `Casks/lumi.rb`. It installs `/Applications/Lumi.app` **and**
the `lumi` CLI the bundle embeds, from `lumi-macos-arm64.zip`, and macOS quarantines it until the
app is notarized. There was a `HomebrewFormula/lumi.rb` carrying the CLI alone through `v0.7.0`;
it was removed because both packages provided a `lumi` command at the same path and Homebrew
refuses to link the second one, so the two could never be installed together. `README.md` carries
the migration for anyone still on it.

The release still publishes `lumi-darwin-arm64.tar.gz` — the bare CLI plus the licence — for people
who want the binary without the app. Nothing in the tap consumes it; it is a manual download.

The cask is rewritten only after the release's assets exist and have been verified, so a failed
release leaves it pointing at the last working one — that is the intent, not an accident. See the
comment block on `update-homebrew-packages` for the recovery,
which is *not* a fresh run: use GitHub's **Re-run failed jobs** on the same run, because
release-please will not re-cut a version whose release already exists.

## Repository secrets

All three are optional. With none of them set the release still ships and `Lumi.app` is ad-hoc
signed; the workflow logs a warning saying so. Set them together or not at all.

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
notarized, and there is no way to opt out of quarantine: `--no-quarantine` no longer exists,
no cask stanza replaced it, and `Cask::Download#quarantine` runs unconditionally.

What one stable certificate still buys, against ad-hoc signing:

| | ad-hoc (`codesign -s -`) | one self-signed certificate |
| --- | --- | --- |
| Designated requirement | the cdhash, new on every build | `certificate leaf = H"…"`, stable |
| TCC grants across an upgrade | all five lost, every release | survive |
| Gatekeeper "Open Anyway" | every upgrade | first install only |

So configure the secrets **before cutting the first cask release**, not after. A bootstrap release
that ships ad-hoc and a second one that ships self-signed differ in designated requirement, which is
`:signer_changed` to Homebrew and a new identity to TCC: every early user loses their permissions and
re-approves, once, for nothing.

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

## Bootstrapping the cask

`Casks/lumi.rb` is committed **after** the first release that carries `lumi-macos-arm64.zip`, and
by hand. The cask's `url` resolves through `v#{version}`, so a cask committed before its asset
exists points the tap at a 404 for every user until the next release, and the reason
`update-homebrew-packages` runs after the assets are published rather than before.
`update-homebrew-packages` maintains the file from the *following* release onward and never authors
the first one. It no longer skips when the file is absent: the cask is the only package the tap
ships, so a missing `Casks/lumi.rb` now fails that job rather than letting a release report success
while pointing users at nothing.

No published release can serve it retroactively. Every release through `v0.5.0` carries only
`lumi-darwin-arm64.tar.gz` and `SHA256SUMS.txt`, and that tarball is the bare CLI binary plus the
licence — there is no bundle in it, and a cask may not build from source.

0. Know that `README.md` already documents `brew install --cask puremetricsai/lumi/lumi`. It ships
   with the workflow that produces the asset, one release ahead of the cask that serves it, so
   between that merge and step 5 below the command fails with "Cask 'lumi' is unavailable". Keep the
   gap to one release.
1. Set the three secrets above. `build-binaries` refuses to run un-signed once `Casks/lumi.rb`
   exists, so this is a precondition of step 5 and not only of step 2.
2. Merge the release PR. Confirm the release carries `lumi-darwin-arm64.tar.gz`,
   `lumi-macos-arm64.zip`, and `SHA256SUMS.txt`, and that `verify-macos-app` passed.
3. Take the ZIP's hash from `SHA256SUMS.txt` and write `Casks/lumi.rb` from the template below,
   with that release's numeric version and that hash.
4. Check it before committing, on macOS. `--strict` alone does not run either of these. Homebrew 6
   refuses a cask outside a tap and has disabled `brew audit` on a path, so copy the candidate into
   the tap clone and check it there by name:

   ```sh
   TAP="$(brew --repository puremetricsai/lumi)"
   mkdir -p "${TAP}/Casks" && cp Casks/lumi.rb "${TAP}/Casks/lumi.rb"
   brew style --cask "${TAP}/Casks/lumi.rb"      # desc, stanza order, deprecated depends_on forms
   brew audit --cask --strict puremetricsai/lumi/lumi
   rm "${TAP}/Casks/lumi.rb"                     # let the push, not the copy, serve it
   ```

   Do **not** pass `--signing` or `--new`. Those are the flags that turn on `audit_signing`, which
   fails by design on an un-notarized bundle.
5. Commit it, then run the measurement below.

### The file to commit

```ruby
cask "lumi" do
  version "0.6.0"
  sha256 "0000000000000000000000000000000000000000000000000000000000000000"

  url "https://github.com/puremetricsai/lumi/releases/download/v#{version}/lumi-macos-arm64.zip"
  name "Lumi"
  desc "Local-first searchable memory of your screen and meetings"
  homepage "https://github.com/puremetricsai/lumi"

  depends_on arch: :arm64
  depends_on macos: :tahoe

  app "Lumi.app"
  binary "#{appdir}/Lumi.app/Contents/MacOS/lumi"

  caveats <<~EOS
    Lumi is not notarized yet, so macOS quarantines it. Open Lumi once from
    Finder, then allow it in System Settings > Privacy & Security > Open Anyway.
    If the `lumi` command reports "Killed: 9", the same approval has not reached
    the embedded binary; clear it with:

      xattr -dr com.apple.quarantine #{appdir}/Lumi.app

    Then grant capture permissions to the installed binary:

      lumi permissions --request
      lumi doctor

    Approve Screen & System Audio Recording, Accessibility, Microphone, and
    Speech Recognition in System Settings > Privacy & Security.
  EOS
end
```

Two of its lines look like free choices and are not, because `brew audit` passes either way and only
`brew style` objects:

- **`depends_on macos: :tahoe`**, not `">= :tahoe"`. The symbol form already means `>=`. The string
  form parses but calls `odeprecated`, which *raises* under `HOMEBREW_DEVELOPER=1`.
- **`desc` may not name the platform.** `Cask/Desc` rejects `Mac`, `macOS`, and `OS X` anywhere in
  the string, so "…for your Mac" fails style while passing audit.

And one absence is deliberate: no `zap`, no `uninstall`, no `postflight`, no `auto_updates`.
Uninstall unlinks the binary on its own and leaves `~/Library/Application Support/Lumi` alone, which
is the point — uninstalling software must never delete captured media or the database. Homebrew owns
upgrades; the app has no updater and is not getting one.

### Measure the bootstrap release

Quarantine has a measured consequence that no amount of caveat wording removes, so find out which
half of it applies before the second release. On a clean Apple Silicon macOS 26 machine:

```sh
brew tap puremetricsai/lumi https://github.com/puremetricsai/lumi
brew install --cask puremetricsai/lumi/lumi
lumi version            # count what happens: output, a prompt, or "Killed: 9"
# then Open Anyway in System Settings, and:
lumi version            # count again
```

A quarantined, never-executed, non-notarized Mach-O is **killed with SIGKILL and no prompt at all**
— measured on macOS 26.5.2, exit 137, no output, `syspolicyd: Terminating process due to Gatekeeper
rejection`. That is the app's first launch, and it is also `lumi` run from a terminal, because
quarantine is written to every file inside the bundle and the cask's `lumi` command is a symlink
into it. What is *not* yet measured is whether approving the app in System Settings also clears the
nested binary. Record the answer here and fix the caveat to match.

Then, on the release after it, check the thing only a second release can show: that a cask upgrade
keeps the designated requirement stable, so the TCC grants and the Gatekeeper approval both survive.

## Recovery

- **The build, signing, upload, or either verification job failed.** The cask still points at the
  previous release, and users are unaffected. Fix forward, or use **Re-run failed
  jobs** on the same run for a transient failure. Do not push a fresh run expecting it to republish:
  release-please sets `release_created` to false for a version whose release exists, and every job
  downstream of it is skipped.
- **The release published but `update-homebrew-packages` failed.** The assets are good; only the tap
  is stale. Re-run that job.
- **`Casks/lumi.rb`'s `version` and `sha256` disagree with the latest tag.** A hand edit landed
  between releases. The next release rewrites both.
- **`Casks/lumi.rb` points at a tag with no `lumi-macos-arm64.zip`.** Every `brew install --cask`
  404s, and that is every install Lumi has. Revert the cask to the previous release's version and
  sha256 by hand.
