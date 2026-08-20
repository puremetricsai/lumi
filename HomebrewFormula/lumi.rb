class Lumi < Formula
  desc "Local-first AI memory CLI for Apple Silicon Macs"
  homepage "https://github.com/puremetricsai/lumi"
  # The `url` and `sha256` lines below are rewritten on every tag by the
  # update-homebrew-packages job in .github/workflows/release-please.yml. Both must exist
  # and stay well-formed: that job seds them in place, it does not insert them, and it
  # first asserts there is exactly one top-level line of each. A `head do`, `stable do`,
  # or `resource` block would introduce a second `url`/`sha256` and fail that assertion.
  url "https://github.com/puremetricsai/lumi/releases/download/v0.6.1/lumi-darwin-arm64.tar.gz"
  sha256 "ab361a848ed4240613a9286a1dac13b71ec437f18bcb1c337a5024adcd6db133"
  license "MIT"

  # Nothing is built here. This is the release archive the build-binaries job produced on a
  # macOS 26 runner, which is the only environment that can produce it: internal/macosnative
  # is `darwin && arm64 && cgo` and links a static Swift archive compiled against the macOS
  # 26 SDK for SpeechAnalyzer. Reproducing that locally is what used to drag the whole Go
  # toolchain onto every installing machine. There is deliberately no `version` line --
  # Homebrew reads 0.4.0 out of the `/download/v0.4.0/` path segment, and stating it again
  # would add a third field the release job has to keep in sync.
  depends_on arch: :arm64
  # A versioned macOS requirement is satisfied on Linux by design, and so is arm64, so
  # without a bare `depends_on :macos` a Linux/arm64 box happily unpacks a Mach-O binary it
  # can never execute and calls the install a success.
  depends_on :macos

  on_macos do
    depends_on macos: :tahoe
  end

  def install
    bin.install "lumi"
    prefix.install "LICENSE"
  end

  def caveats
    <<~EOS
      macOS grants capture permissions to a specific binary, so grant them to the
      installed one:

        #{opt_bin}/lumi permissions --request
        #{opt_bin}/lumi doctor

      Approve Screen & System Audio Recording, Accessibility, Microphone, and Speech
      Recognition in System Settings > Privacy & Security.
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/lumi version")
    assert_match "Searchable, local-first work activity", shell_output("#{bin}/lumi --help")
  end
end
