class Lumi < Formula
  desc "Local-first AI memory CLI for Apple Silicon Macs"
  homepage "https://github.com/puremetricsai/lumi"
  # The `url` and `sha256` lines below are rewritten on every tag by the
  # update-homebrew-formula job in .github/workflows/release-please.yml. Both must exist
  # and stay well-formed: that job seds them in place, it does not insert them.
  url "https://github.com/puremetricsai/lumi/archive/refs/tags/v0.1.0.tar.gz"
  sha256 "c3b7409dd99e1bf26865bb60c685956f6ef72b9e282ed584c9dcd44a419cc4ce"
  license "MIT"
  head "https://github.com/puremetricsai/lumi.git", branch: "main"

  depends_on "go" => :build
  depends_on arch: :arm64
  # A versioned macOS requirement is satisfied on Linux by design, and so is arm64, so
  # without a bare `depends_on :macos` a Linux/arm64 box happily starts the install,
  # pulls the keg-only homebrew-core `swift`, and then dies at `system "swiftc"` with a
  # message that hides the real reason: internal/macosnative is `darwin && arm64 && cgo`.
  depends_on :macos
  depends_on macos: :tahoe

  # swiftc ships with the Command Line Tools that Homebrew already requires, so this only
  # documents the toolchain and installs nothing. Do not use `depends_on xcode:` — it
  # demands a full Xcode.app, which Lumi does not need.
  uses_from_macos "swift" => :build

  def install
    # internal/macosnative links `-L${SRCDIR} -llumispeech`, so the static Swift bridge
    # must exist inside the source tree before cgo runs. Mirrors the Taskfile's
    # `speech` -> `build` ordering.
    system "swiftc", "-emit-library", "-static", "-O",
           "-o", "internal/macosnative/liblumispeech.a",
           "internal/macosnative/speech.swift"

    ldflags = %W[
      -s -w
      -X github.com/puremetricsai/lumi/internal/cli.version=v#{version}
    ]
    ENV["CGO_ENABLED"] = "1"
    system "go", "build", *std_go_args(ldflags: ldflags), "./cmd/lumi"
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
