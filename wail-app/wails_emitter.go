package main

import (
	"github.com/wailsapp/wails/v3/pkg/application"
)

// WailsEmitter implements EventEmitter using the Wails v3 event system.
type WailsEmitter struct {
	app *application.App
}

// NewWailsEmitter creates a new Wails event emitter.
func NewWailsEmitter(app *application.App) *WailsEmitter {
	return &WailsEmitter{app: app}
}

// Emit sends an event to the frontend.
func (e *WailsEmitter) Emit(event string, data any) {
	e.app.Event.Emit(event, data)
}
