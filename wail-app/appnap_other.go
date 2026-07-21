//go:build !darwin

package main

// disableAppNap is a no-op off macOS (App Nap is a macOS mechanism).
func disableAppNap() {}
