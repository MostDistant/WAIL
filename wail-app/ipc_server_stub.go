//go:build linkstub

package main

import "context"

// maybeStartIPCServer is a no-op under -tags linkstub: the stub build has no audio
// engine to bridge to, so there is nothing for a CLAP plugin to connect to.
func maybeStartIPCServer(_ context.Context, _ uint16, _ AudioEngine) func() {
	return func() {}
}
