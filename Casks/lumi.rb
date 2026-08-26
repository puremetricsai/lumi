cask "lumi" do
  version "0.2.0"
  sha256 "7e8f312509bd712fcc1576ce49c0108c92d2cd56783e2d266d091ce7ee8b0057"

  url "https://github.com/puremetricsai/lumi/releases/download/v#{version}/lumi-macos-arm64.zip"
  name "Lumi"
  desc "Local-first searchable memory of your screen and meetings"
  homepage "https://github.com/puremetricsai/lumi"

  depends_on arch: :arm64
  depends_on macos: :tahoe

  app "Lumi.app"

  caveats <<~EOS
    Lumi is not notarized yet, so macOS quarantines it. Open Lumi once from
    Finder, then allow it in System Settings > Privacy & Security > Open Anyway.
    If it still will not launch, the approval has not reached everything inside
    the bundle; clear the flag with:

      xattr -dr com.apple.quarantine #{appdir}/Lumi.app

    Then open Lumi > Settings > Permissions and grant Screen & System Audio
    Recording, Accessibility, Microphone, and Speech Recognition.

    Upgrading from 0.2.0 or earlier: this cask no longer puts a `lumi` command
    on your PATH, so AI agents registered against that path stop resolving.
    Re-register them from Lumi > Settings > MCP, which offers Replace for an
    entry still naming the old one.
  EOS
end
