# Signing and Notarizing Lumi

How to configure an Apple Developer account, create credentials, sign, notarize, staple, and distribute Lumi for macOS.

## 1. Apple Developer Account Setup

Signing and notarization require a paid Apple Developer Program account ($99/yr).

### Roles
- **Account Holder**: Required to create Developer ID certificates.
- **Admin**: Required to generate App Store Connect Team API keys.

---

### Step 1: Create Developer ID Application Certificate
1. Open **Keychain Access** $\rightarrow$ **Certificate Assistant** $\rightarrow$ **Request a Certificate from a Certificate Authority...**
   - User Email Address: Your Apple ID email
   - Common Name: `Lumi Developer ID Key`
   - Request is: **Saved to disk**
   - Saves `CertificateSigningRequest.certSigningRequest`.
2. Sign in to [Apple Developer: Certificates](https://developer.apple.com/account/resources/certificates/list).
3. Click **(+)** $\rightarrow$ Select **Developer ID Application** (under Software) $\rightarrow$ **Continue**.
4. Upload `.certSigningRequest` $\rightarrow$ **Generate** $\rightarrow$ Download `developerID_application.cer`.
5. Install the certificate **into the login keychain**, the one holding the private key the CSR in
   step 1 created. Double-clicking the `.cer` can drop it into the System keychain instead, and an
   identity only exists when certificate and key share a keychain — so the certificate lands under
   **Certificates** rather than **My Certificates**, *Export as .p12* is grayed out, and
   `security find-identity -p codesigning` finds nothing to export. Import explicitly:
   ```sh
   security import developerID_application.cer -k ~/Library/Keychains/login.keychain-db
   ```
6. Install Apple's **Developer ID Certification Authority (G2)** intermediate. It is not present by
   default, and without it the certificate reads as untrusted — Keychain Access says so, and
   `security find-identity -p codesigning` reports the identity as `CSSMERR_TP_NOT_TRUSTED` while
   `find-identity -v` reports `0 valid identities found`:
   ```sh
   curl -fsSLO https://www.apple.com/certificateauthority/DeveloperIDG2CA.cer
   security import DeveloperIDG2CA.cer -k ~/Library/Keychains/login.keychain-db
   ```
7. Confirm both steps took, before going further:
   ```sh
   security find-identity -v -p codesigning   # lists the identity exactly once
   ```
   Two entries mean a stale copy in another keychain; delete that one, naming its keychain file:
   `sudo security delete-certificate -Z <SHA-1> /Library/Keychains/System.keychain`.
8. Under **My Certificates**, find:
   `Developer ID Application: <Team Name> (<TEAM_ID>)`
9. Right-click certificate $\rightarrow$ **Export "Developer ID Application: ..."** as `.p12` with a password,
   or export from the command line:
   ```sh
   security export -k ~/Library/Keychains/login.keychain-db -t identities -f pkcs12 -o DeveloperID.p12
   ```
10. Base64 encode for CI:
   ```sh
   base64 -i DeveloperID.p12 | pbcopy
   ```
11. The first `codesign` run prompts for keychain access. Answer **Always Allow** — plain *Allow*
   re-prompts on every signature, and `build-app.sh` makes two per build, so it stalls mid-build
   waiting on a dialog. The non-interactive equivalent is
   `security set-key-partition-list -S apple-tool:,apple:,codesign: -s -k <login-password> ~/Library/Keychains/login.keychain-db`.

---

### Step 2: Create notarization credentials

`notarytool` authenticates three ways, and the choice is not cosmetic — an App Store Connect API key
needs App Store Connect access, which Developer Program enrollment does not by itself grant. If
App Store Connect answers *"Your Apple Account isn't enabled for App Store Connect"*, use Step 2b;
it needs nothing from App Store Connect at all.

| Method | Flags | Needs App Store Connect |
| :--- | :--- | :--- |
| API key (**preferred**) | `--key` `--key-id` `--issuer` | yes |
| Apple ID (**interim**) | `--apple-id` `--password` `--team-id` | no |
| Keychain profile | `--keychain-profile` | wraps either of the above |

Prefer the API key: it is not tied to one person's Apple ID, and
`.claude/commands/lumi-developer-id-signing.md` names it as the target state. Step 2b is the
workaround for a blocked account, not the destination.

#### Step 2a: App Store Connect API key
> **Note**: `--issuer` is **required for Team keys and must be omitted for Individual keys**. Passing
> it with an Individual key is a 401. Prefer a Team key; the workflow passes `--issuer`.

1. Sign in to [App Store Connect: Users and Access $\rightarrow$ Integrations $\rightarrow$ App Store Connect API](https://appstoreconnect.apple.com/access/integrations/api).
2. Click **(+)** to generate a new key:
   - Name: `Lumi Notarization Key`
   - Access: `Developer` (or `App Manager`)
3. Record:
   - **Issuer ID**: UUID at top of page (e.g. `57246542-96fe-1a63-e053-0824d011072a`)
   - **Key ID**: 10-character string in table (e.g. `2X9R4HXF34`)
4. Download `AuthKey_<KEY_ID>.p8` (only downloadable once).
5. Base64 encode for CI:
   ```sh
   base64 -i AuthKey_<KEY_ID>.p8 | pbcopy
   ```

#### Step 2b: Apple ID and app-specific password

Use this only when Step 2a is blocked. An app-specific password is created on the Apple ID account
page, which every Developer Program member can reach:

1. Sign in to [account.apple.com](https://account.apple.com) $\rightarrow$ **Sign-In and Security**
   $\rightarrow$ **App-Specific Passwords**.
2. Generate one named `Lumi Notarization`. It is shown once.
3. The **Team ID** is the parenthesised code in the certificate name —
   `Developer ID Application: <Team Name> (<TEAM_ID>)`.

```sh
xcrun notarytool submit lumi-submission.zip \
  --apple-id <apple-id-email> --team-id <TEAM_ID> --password <app-specific-password> --wait
```

Revoking the password, or the person leaving the team, breaks the release. That is the reason to
move to Step 2a once App Store Connect is reachable.

---

### Step 3: Configure GitHub Repository Secrets
Add to GitHub repository **Settings $\rightarrow$ Secrets and variables $\rightarrow$ Actions**:

| Secret | Value |
| :--- | :--- |
| `MACOS_DEVELOPER_ID_P12_BASE64` | Base64-encoded `.p12` file |
| `MACOS_DEVELOPER_ID_P12_PASSWORD` | Password protecting `.p12` |
| `MACOS_DEVELOPER_IDENTITY` | Exact certificate name: `Developer ID Application: <Team Name> (<TEAM_ID>)` |
| `APPLE_API_KEY_P8_BASE64` | Base64-encoded `AuthKey_<KEY_ID>.p8` |
| `APPLE_API_KEY_ID` | 10-character Key ID |
| `APPLE_API_ISSUER_ID` | Issuer UUID |

The last three are Step 2a. Having used Step 2b instead, set these three in their place, and the
workflow's notarization step must pass the Apple ID flags rather than the API-key ones:

| Secret | Value |
| :--- | :--- |
| `APPLE_ID` | Apple ID email the app-specific password belongs to |
| `APPLE_APP_SPECIFIC_PASSWORD` | the app-specific password from Step 2b |
| `APPLE_TEAM_ID` | `<TEAM_ID>`, the code in the certificate name |

---

## 2. Hardened Runtime and Entitlements

Lumi uses:
- **Hardened Runtime (`--options runtime`)**: Required for notarization. Go + Cgo + static Swift `liblumispeech.a` runs without JIT, unsigned executable memory, or library validation disablement exceptions.
- **No App Sandbox**: Omitted because system-wide Accessibility attribution (`AXIsProcessTrustedWithOptions`) and background capture across apps cannot run in App Sandbox.
- **Entitlements (`macos/Lumi/Resources/Lumi.entitlements`)**: one key,
  `com.apple.security.device.audio-input`. Without it the Hardened Runtime denies the microphone
  *silently* — the prompt still appears, the user still allows it, and `AVCaptureDevice` keeps
  answering `not_determined`. Nothing errors, and Lumi never appears in System Settings > Privacy &
  Security > Microphone. Nothing else Lumi does needs a key here: screen and system-audio capture
  are governed by the Screen Recording TCC service, and Accessibility, Vision, SpeechAnalyzer and
  Speech Recognition have no Hardened Runtime entitlement at all. `build-app.sh` passes the file to
  every signature and then asserts the built bundle carries the key.
- **Info.plist Purpose Strings**:
  - `NSMicrophoneUsageDescription`
  - `NSSpeechRecognitionUsageDescription`
  - `NSScreenCaptureUsageDescription`
  - `NSAppleEventsUsageDescription`

---

## 3. Manual Signing & Notarization Commands

```sh
# 1. Build
task build && task app

# 2. Sign the bundle with runtime + timestamp. build-app.sh signs the embedded
#    binary before the bundle around it; nothing is signed or shipped separately.
IDENTITY="Developer ID Application: Puremetrics AI Inc. (ABC123XYZ0)"
CODESIGN_IDENTITY="$IDENTITY" ./macos/build-app.sh

# 3. Create submission archive
ditto -c -k --keepParent build/Lumi.app lumi-submission.zip

# 4. Submit to Apple Notary Service
xcrun notarytool submit lumi-submission.zip \
  --key /path/to/AuthKey_KEYID.p8 \
  --key-id KEYID \
  --issuer ISSUER_UUID \
  --wait

# 5. Staple ticket to app bundle
xcrun stapler staple build/Lumi.app
xcrun stapler validate build/Lumi.app

# 6. Verify Gatekeeper acceptance
spctl --assess --type execute --verbose=2 build/Lumi.app
codesign --verify --deep --strict --verbose=2 build/Lumi.app

# 7. Final distributable zip
ditto -c -k --keepParent build/Lumi.app lumi-macos-arm64.zip
```

---

## 4. CI Release Workflow Integration

In `.github/workflows/release-please.yml`:

1. **Import Keychain & API Key**: Decode `.p12` into temporary keychain and `.p8` with mode 0600.
   Do not import `DeveloperIDG2CA.cer` in CI, with a real Developer ID identity or without one:
   on `macos-26` `security import` exits 1 with "The specified item already exists in the keychain"
   and fails the release (run 33126959558) — the intermediate is already reachable there. Should a
   future runner image drop it, tolerate the duplicate rather than letting it fail the step:
   `security import ... || grep -q "already exists" <<<"$out"`. A local machine missing the
   intermediate is the case section 2 covers.
2. **Build & Sign App**: Run `CODESIGN_IDENTITY="$IDENTITY" ./macos/build-app.sh`. It signs the embedded binary and then the bundle. `Lumi.app` is the only artifact — there is nothing to sign or notarize beside it.
3. **Notarize & Staple App**:
   - `ditto -c -k --keepParent build/Lumi.app lumi-submission.zip`
   - `xcrun notarytool submit lumi-submission.zip ... --wait --output-format json`
   - On error: fetch submission log via `xcrun notarytool log <submission-id> ...`
   - `xcrun stapler staple build/Lumi.app && xcrun stapler validate build/Lumi.app`
   - Re-archive final `lumi-macos-arm64.zip`.
4. **Verify**: Run `codesign --verify --deep --strict`, `spctl --assess --type execute` on both `Lumi.app` and `Contents/MacOS/lumi`.
5. **Cleanup**: Delete temporary keychain and private key files in `if: always()`.

---

## 5. Downstream Updates

1. **`README.md`**:
   - Remove the "The app is not notarized yet" section: the browser-download recovery it documents
     stops applying once the bundle is stapled.
2. **TCC Grants**:
   - Upgrading from interim self-signed to Developer ID resets TCC permissions once.
   - Subsequent updates retain permissions stably under `Developer ID Application` Designated Requirement.
