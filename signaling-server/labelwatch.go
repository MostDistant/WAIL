package main

import (
	"encoding/binary"
	"log"
	"sync"
	"time"
)

// Label watchdog (field finding, v3.12.3 jam): a peer whose server↔local
// clock offset estimate went bad (stalled pong — laptop sleep, Wi-Fi stall)
// derives a room-label offset that is wrong by whole intervals, and because
// anchors only re-broadcast on tempo/config change, the error is FROZEN for
// the session: the peer's frames are labeled k intervals ahead/behind, and
// playout holds or drops them — the peer is silent or misaligned for
// everyone. The mislabeled peer cannot self-diagnose (both sides see
// symmetric disagreement); the relay owns the room index and is the only
// party with ground truth.
//
// The watchdog peeks at each audio frame's WAIF header as it is broadcast,
// compares the sender's interval label against the room index, and on a
// sustained mismatch unicasts a fresh interval_anchor to the offending peer
// (unicast, not broadcast: re-anchors re-roll every client's labeler). The
// peer re-derives and heals mid-jam. All events are logged for fly.io review.

const (
	// watchLabelThreshold is the |label − room index| beyond which a peer is
	// considered misaligned. ±1 is normal boundary straddle; D is at most a
	// few intervals of in-flight buffering.
	watchLabelThreshold = 2
	// watchSustainFrames is how many consecutive offending frames trigger an
	// anchor re-send (~2s at 50 fps — long enough to ignore join turbulence).
	watchSustainFrames = 100
	// watchCooldown is the minimum time between anchor re-sends per peer.
	watchCooldown = 60 * time.Second
)

// waifIntervalIndex peeks the interval_index field of a WAIF frame (wire.go:
// 4 magic + 1 flags + 2 stream_id + 8 interval_index at [7:15]).
func waifIntervalIndex(data []byte) (int64, bool) {
	if len(data) < 15 || string(data[0:4]) != "WAIF" {
		return 0, false
	}
	return int64(binary.LittleEndian.Uint64(data[7:15])), true
}

type peerLabelState struct {
	offending int       // consecutive offending frames
	lastK     int64     // last mismatch magnitude (for logging)
	flagged   bool      // currently flagged misaligned
	lastSent  time.Time // last anchor re-send (cooldown)
}

type labelWatchdog struct {
	mu    sync.Mutex
	peers map[string]*peerLabelState
}

func newLabelWatchdog() *labelWatchdog {
	return &labelWatchdog{peers: make(map[string]*peerLabelState)}
}

// observe processes one audio frame from peerID. roomIdx is the relay's
// authoritative room index now; sendAnchor re-sends the current anchor to
// this peer only. roomName/peerID are for logging.
func (w *labelWatchdog) observe(roomName, peerID string, label, roomIdx int64, now time.Time, sendAnchor func()) {
	k := label - roomIdx
	misaligned := k > watchLabelThreshold || k < -watchLabelThreshold

	w.mu.Lock()
	st := w.peers[peerID]
	if st == nil {
		st = &peerLabelState{}
		w.peers[peerID] = st
	}
	if !misaligned {
		if st.flagged {
			log.Printf("[watchdog] room %s peer %s healed (labels back within ±%d of room index %d)", roomName, peerID, watchLabelThreshold, roomIdx)
			st.flagged = false
		}
		st.offending = 0
		w.mu.Unlock()
		return
	}
	st.offending++
	st.lastK = k
	if st.offending >= watchSustainFrames && (st.lastSent.IsZero() || now.Sub(st.lastSent) >= watchCooldown) {
		st.lastSent = now
		if !st.flagged {
			st.flagged = true
			log.Printf("[watchdog] room %s peer %s labels off by %+d intervals (frame %d vs room %d, %d consecutive frames) — sent fresh anchor",
				roomName, peerID, k, label, roomIdx, st.offending)
		} else {
			log.Printf("[watchdog] room %s peer %s STILL off by %+d after anchor re-send — sending again", roomName, peerID, k)
		}
		w.mu.Unlock()
		sendAnchor()
		return
	}
	w.mu.Unlock()
}

// forget drops a peer's state on leave/disconnect.
func (w *labelWatchdog) forget(peerID string) {
	w.mu.Lock()
	delete(w.peers, peerID)
	w.mu.Unlock()
}
