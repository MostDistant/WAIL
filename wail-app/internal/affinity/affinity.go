// Package affinity identifies a remote stream by its owner's persistent identity
// and keeps the channel name it is published under stable.
//
// A musician who reconnects gets a new session peer ID but the same persistent
// identity, so keying per-stream state by Key means a reconnecting peer reuses
// the *same* published Link Audio channel rather than minting a fresh one — LAN
// apps' routing survives the blip (CONTEXT.md: channel affinity, degrade
// gracefully). The engine holds that per-stream state (audio_engine_real.go); no
// separate registry is needed.
package affinity

import "strings"

// Key identifies one remote stream by its owner's persistent identity and the
// stream index within that peer.
type Key struct {
	Identity string
	Stream   uint16
}

// FormatName builds a channel display name from peer and stream names. Missing
// parts degrade gracefully so the LAN never sees an empty or " · " name.
func FormatName(peerName, streamName string) string {
	p := strings.TrimSpace(peerName)
	s := strings.TrimSpace(streamName)
	switch {
	case p == "" && s == "":
		return "WAIL"
	case p == "":
		return s
	case s == "":
		return p
	default:
		return p + " · " + s
	}
}
