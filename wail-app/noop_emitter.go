package main

import "log"

// NoopEmitter implements EventEmitter without a GUI frontend.
// Used in headless CLI mode.
type NoopEmitter struct{}

// silentEvents are high-frequency or routine events that add noise in headless mode.
// Noteworthy events (errors, peer join/leave, session lifecycle, plugin disconnect)
// are still logged.
var silentEvents = map[string]bool{
	"debug:interval-frame": true,
	"debug:link-tick":      true,
	"status:update":        true,
	"peers:network":        true,
	"log:entry":            true,
}

func (e *NoopEmitter) Emit(event string, data any) {
	if silentEvents[event] {
		return
	}
	log.Printf("[event] %s", event)
}

func (e *NoopEmitter) Shutdown() {}
