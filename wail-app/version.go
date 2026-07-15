package main

// appVersion is injected at build time via -ldflags from the release git tag
// (see .github/workflows/release.yml). Unreleased/local builds show the default.
var appVersion = "0.0.0-dev"
