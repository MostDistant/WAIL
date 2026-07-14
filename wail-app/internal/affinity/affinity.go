// Package affinity maps a remote (persistent identity, stream index) to a stable
// published Link Audio channel, so a musician who reconnects (new session peer
// ID, same identity) has their streams reappear as the *same* channels rather
// than freshly-minted ones — LAN apps' routing survives the blip (CONTEXT.md:
// channel affinity, degrade gracefully).
//
// The registry is generic over the channel handle type (the LinkAudioSink in
// production) so it stays pure and unit-testable. Not safe for concurrent use;
// drive it from a single goroutine.
package affinity

import "strings"

// Key identifies one remote stream by its owner's persistent identity and the
// stream index within that peer.
type Key struct {
	Identity string
	Stream   uint16
}

// Entry is a live published channel for one Key.
type Entry[H any] struct {
	Key    Key
	Name   string // display name: "{peer} · {stream}"
	Handle H
}

// Registry tracks published channels keyed by (identity, stream).
type Registry[H any] struct {
	entries map[Key]*Entry[H]
}

// New creates an empty registry.
func New[H any]() *Registry[H] {
	return &Registry[H]{entries: make(map[Key]*Entry[H])}
}

// Resolve returns the channel for key, creating it via create() on first use.
// The bool is true only when a new channel was created; on a reconnect it is
// false and the existing handle is reused (affinity). The display name is
// refreshed each call so peer/stream renames are reflected without re-minting
// the channel.
func (r *Registry[H]) Resolve(key Key, peerName, streamName string, create func() H) (*Entry[H], bool) {
	name := FormatName(peerName, streamName)
	if e, ok := r.entries[key]; ok {
		e.Name = name
		return e, false
	}
	e := &Entry[H]{Key: key, Name: name, Handle: create()}
	r.entries[key] = e
	return e, true
}

// Get returns the existing entry for key, if any.
func (r *Registry[H]) Get(key Key) (*Entry[H], bool) {
	e, ok := r.entries[key]
	return e, ok
}

// Remove deletes the entry for key and returns its handle so the caller can tear
// down the underlying channel.
func (r *Registry[H]) Remove(key Key) (H, bool) {
	e, ok := r.entries[key]
	if !ok {
		var zero H
		return zero, false
	}
	delete(r.entries, key)
	return e.Handle, true
}

// Keys returns all live keys (order unspecified).
func (r *Registry[H]) Keys() []Key {
	out := make([]Key, 0, len(r.entries))
	for k := range r.entries {
		out = append(out, k)
	}
	return out
}

// Len is the number of live channels.
func (r *Registry[H]) Len() int { return len(r.entries) }

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
