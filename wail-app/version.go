package main

// appVersion is injected at build time via -ldflags from Cargo.toml workspace
// version. Unreleased/local builds show the default.
var appVersion = "0.0.0-dev"
