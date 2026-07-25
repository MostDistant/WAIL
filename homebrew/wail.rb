# WAIL Homebrew Formula
#
# This file is the source of truth for the Homebrew formula.
# It is copied automatically to the MostDistant/homebrew-wail tap on each release.
# The `url` and `sha256` fields below are updated by the release workflow.
#
# Architecture: Go/Wails desktop app.
# The app (session orchestration, Link sync, signaling, audio) is built with Go,
# with an Ableton Link binding compiled via cgo against the vendor/link submodule.
#
# To install:
#   brew tap MostDistant/wail
#   brew install MostDistant/wail/wail

class Wail < Formula
  desc "Sync Ableton Link sessions across the internet with intervalic audio"
  homepage "https://github.com/MostDistant/WAIL"
  # url and sha256 are updated automatically by the release workflow
  url "https://github.com/MostDistant/WAIL/releases/download/v0.4.5/wail-0.4.5-src.tar.gz"
  sha256 "0000000000000000000000000000000000000000000000000000000000000000"
  license "MIT"
  head "https://github.com/MostDistant/WAIL.git", branch: "main", submodules: true

  depends_on "cmake" => :build
  depends_on "go" => :build
  depends_on "pkg-config" => :build
  depends_on "opus"
  depends_on :macos # requires macOS WebKit (used by Wails webview)

  def install
    # Homebrew's superenv pkg-config shim references the legacy "pkg-config"
    # opt path, but modern Homebrew provides it via "pkgconf". Point pkg-config
    # directly at the real binary so the Go/cgo opus binding finds Opus.
    ENV["PKG_CONFIG"] = Formula["pkgconf"].opt_bin/"pkg-config"

    # Head builds are a bare git checkout; fetch the vendor/link submodule the
    # cgo Ableton Link binding compiles against. Release tarballs bundle it.
    system "git", "submodule", "update", "--init", "--recursive" if build.head?

    # Build the Go/Wails desktop app. appVersion (from the release `url`) is
    # injected so the UI shows the installed version.
    cd "wail-app" do
      system "go", "build", "-tags", "nolibopusfile", "-ldflags", "-X main.appVersion=#{version}", "-o", "wail", "."
    end
    bin.install "wail-app/wail"

    # Build the CLAP plugins (thin PCM bridge for non-Link-Audio DAWs, ADR-0005) and
    # stage them under lib/. Homebrew can't write into the user's plugin folder, so
    # `wail-install-plugins` (below) copies them there on demand.
    system "cmake", "-S", "plugins", "-B", "build/plugins", "-DCMAKE_BUILD_TYPE=Release"
    system "cmake", "--build", "build/plugins"
    # Product bundles only: dev tools (transport-probe, linkbridge-spike) build
    # alongside but must not be installed.
    lib.install Dir["build/plugins/{wail-send,wail-recv,linkbridge-send,linkbridge-recv}.clap"]
    bin.install "scripts/wail-install-plugins.sh" => "wail-install-plugins"
  end

  def caveats
    <<~EOS
      The WAIL CLAP plugins were built but not copied into your DAW plugin folder
      (Homebrew can't write there). Install them with:
        wail-install-plugins
      Then rescan plugins in your DAW. You only need them for DAWs without native
      Ableton Link Audio (Live 12.3+ needs no plugin).
    EOS
  end

  test do
    assert_predicate bin/"wail", :exist?
  end
end
