package main

import (
	"github.com/honeybadger-io/honeybadger-go"
)

const honeybadgerAPIKey = "hbp_0GYAg4zTkkp5dnhFf4k3Ke9rvmfvA62vKf8O"

// InitHoneybadger configures Honeybadger error reporting.
// Call defer honeybadger.Monitor() in main() after this.
func InitHoneybadger() {
	honeybadger.Configure(honeybadger.Configuration{
		APIKey: honeybadgerAPIKey,
		Env:    "production",
	})
}

// ReportError reports an error to Honeybadger asynchronously.
func ReportError(err error) {
	honeybadger.Notify(err)
}

// FlushHoneybadger ensures all pending reports are sent before shutdown.
func FlushHoneybadger() {
	honeybadger.Flush()
}
