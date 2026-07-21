package affinity

// OwnChannels classifies discovered Link Audio channels as this peer's own
// republished sinks, so capture discovery can exclude them without hiding a
// third-party publisher that merely shares our Link peer name (matching by
// peer name alone blanked the capture list when a DAW plugin defaulted its
// peer name to the machine name).
//
// The sink channel ID is not exposed by the abl_link C API, so it is learned:
// every sink name WAIL mints is recorded via Published, and the first
// discovery snapshot that pairs a minted name with our own peer name teaches
// us that channel's ID. From then on the ID alone is decisive — rename-proof
// and collision-proof.
//
// Not goroutine-safe: the engine calls it under its own mutex.
type OwnChannels struct {
	names map[string]bool // every sink name we ever minted this session
	ids   map[string]bool // channel IDs (hex) learned to be ours
}

// NewOwnChannels creates an empty classifier.
func NewOwnChannels() *OwnChannels {
	return &OwnChannels{names: make(map[string]bool), ids: make(map[string]bool)}
}

// Published records a sink name this peer minted (call on sink create and on
// every rename). Names accumulate: a stale discovery snapshot may still carry
// a pre-rename name.
func (o *OwnChannels) Published(name string) {
	o.names[name] = true
}

// Own reports whether a discovered channel is one of ours. id is the channel
// ID in hex; peerNameMatches is whether the channel's peer name equals our
// own. A minted-name + peer-name match teaches the classifier the ID.
func (o *OwnChannels) Own(id string, peerNameMatches bool, name string) bool {
	if o.ids[id] {
		return true
	}
	if peerNameMatches && o.names[name] {
		o.ids[id] = true
		return true
	}
	return false
}
