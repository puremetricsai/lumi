cask "lumi" do
  version "0.1.0"
  sha256 "4cfcfe67dde32a6917c81d590a531893b7c4ee070aa48c33da8b3919b025b242"

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
