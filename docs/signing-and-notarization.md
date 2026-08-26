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
5. Double-click `developerID_application.cer` to install into Keychain Access.
6. Under **My Certificates**, find:
   `Developer ID Application: <Team Name> (<TEAM_ID>)`
7. Right-click certificate $\rightarrow$ **Export "Developer ID Application: ..."** as `.p12` with a password.
8. Base64 encode for CI:
   ```sh
   base64 -i DeveloperID.p12 | pbcopy
   ```

---

### Step 2: Create App Store Connect Team API Key
> **Note**: `notarytool` requires an **App Store Connect Team Key** (Individual keys fail with 401).

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

1. **`Casks/lumi.rb`**:
   - Remove "Open Anyway" / `xattr -dr com.apple.quarantine` workarounds.
   - Retain permissions guidance (`lumi permissions --request`).
2. **TCC Grants**:
   - Upgrading from interim self-signed to Developer ID resets TCC permissions once.
   - Subsequent updates retain permissions stably under `Developer ID Application` Designated Requirement.
