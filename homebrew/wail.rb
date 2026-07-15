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
  end

  test do
    assert_predicate bin/"wail", :exist?
  end
end
