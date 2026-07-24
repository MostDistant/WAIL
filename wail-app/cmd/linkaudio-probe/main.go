// linkaudio-probe is a DAW-free Link Audio consumer for testing. It is a Link
// Audio peer that subscribes to every discovered channel and, once per second,
// reports what it received per channel: buffers, frames, RMS (is it non-silent?),
// an estimated dominant frequency (zero-crossing), and cumulative LAN-loss from
// the per-buffer count sequence.
//
// Subscribing is what *activates* a WAIL sink (Link Audio only sends while a
// source is present), so this tool both drives and verifies WAIL's emit path
// without needing Ableton/Bitwig. Feed WAIL a rising sweep (cmd/gen-sweep) and
// the reported frequency should climb monotonically — proof the received audio
// is intact and in order.
package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nicholasgasior/wail/wail-app/internal/abllink"
	"github.com/nicholasgasior/wail/wail-app/internal/lanloss"
)

type chanState struct {
	src  *abllink.Source
	name string
	peer string
	loss lanloss.Tracker

	// per-tick accumulators
	buffers    int
	frames     int
	sumSq      float64
	zeroCross  int
	sampleRate uint32
	channels   int
	// gridFrames counts received frames per shared-grid interval index (the
	// BPI-lens beat the buffer begins at, floored to its interval). Only
	// populated when -bpi is set; used by the tier2 interval-placement E2E.
	gridFrames map[int64]int
}

func main() {
	peerName := flag.String("name", "wail-probe", "Link Audio peer name for this probe")
	match := flag.String("match", "", "only subscribe to channels whose \"peer · name\" contains this substring (case-insensitive); empty = all")
	bpi := flag.Float64("bpi", 0, "if >0, also report the shared-grid interval index of received audio (beat mapped at this beats-per-interval lens; must match the room's BPI)")
	flag.Parse()
	matchLower := strings.ToLower(*match)

	l := abllink.New(120)
	l.SetPeerName(*peerName)
	l.Enable(true)
	l.EnableLinkAudio(true)
	defer l.Close()

	var ss *abllink.SessionState
	if *bpi > 0 {
		ss = abllink.NewSessionState()
	}

	subs := make(map[abllink.ChannelID]*chanState)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	// Drain fast so the source ring never overflows; report once per second.
	const drainEvery = 15 * time.Millisecond
	ticker := time.NewTicker(drainEvery)
	defer ticker.Stop()
	reportEvery := int((1 * time.Second) / drainEvery)
	tick := 0

	log.Printf("linkaudio-probe listening as %q — subscribing to all discovered channels (Ctrl-C to stop)", *peerName)

	for {
		select {
		case <-sigCh:
			log.Printf("stopping")
			for _, st := range subs {
				st.src.Close()
			}
			return

		case <-ticker.C:
			tick++
			report := tick%reportEvery == 0
			// Discover + subscribe to any new channels (skip our own). Once/sec
			// is plenty for discovery.
			if report {
				for _, c := range l.Channels() {
					if c.PeerName == *peerName {
						continue
					}
					if matchLower != "" && !strings.Contains(strings.ToLower(c.PeerName+" · "+c.Name), matchLower) {
						continue
					}
					if _, ok := subs[c.ID]; ok {
						continue
					}
					src := l.NewSource(c.ID, 512, 0)
					if src == nil {
						continue
					}
					subs[c.ID] = &chanState{src: src, name: c.Name, peer: c.PeerName, gridFrames: map[int64]int{}}
					log.Printf("→ subscribed: %q · %q", c.PeerName, c.Name)
				}
			}

			// Drain EVERY tick so the source ring never overflows.
			if ss != nil {
				l.CaptureAppSessionState(ss)
			}
			for _, st := range subs {
				for {
					buf, ok := st.src.Pop()
					if !ok {
						break
					}
					st.buffers++
					st.frames += buf.NumFrames
					st.sampleRate = buf.SampleRate
					ch := buf.NumChannels
					if ch < 1 {
						ch = 1
					}
					st.channels = ch
					if ss != nil {
						// Map the buffer's begin onto the shared session grid at
						// the BPI lens (the interval grid is session-shared,
						// ADR-0003) and bin its frames by interval index.
						if bb, ok := st.src.BeginBeats(&buf, ss, *bpi); ok {
							st.gridFrames[int64(math.Floor(bb / *bpi))] += buf.NumFrames
						}
					}
					if g := st.loss.Observe(buf.Count); g != nil {
						log.Printf("  ! LAN loss on %q · %q: %d buffers (count %d→%d)",
							st.peer, st.name, g.LostBuffers, g.ExpectedCount, g.GotCount)
					}
					// RMS + zero-crossings on channel 0.
					var prev int16
					first := true
					for i := 0; i < buf.NumFrames; i++ {
						idx := i * ch
						if idx >= len(buf.Samples) {
							break
						}
						s := buf.Samples[idx]
						st.sumSq += float64(s) * float64(s)
						if !first && (prev < 0) != (s < 0) {
							st.zeroCross++
						}
						prev = s
						first = false
					}
				}
			}

			if !report {
				continue
			}

			for _, st := range subs {
				if st.buffers == 0 {
					log.Printf("[%s · %s] silent (no buffers this second)", st.peer, st.name)
				} else {
					rms := 0.0
					if st.frames > 0 {
						rms = math.Sqrt(st.sumSq / float64(st.frames))
					}
					freq := 0.0
					if st.frames > 0 && st.sampleRate > 0 {
						freq = (float64(st.zeroCross) / 2.0) / (float64(st.frames) / float64(st.sampleRate))
					}
					grid := ""
					if len(st.gridFrames) > 0 {
						var bestG int64
						bestN := -1
						for g, n := range st.gridFrames {
							if n > bestN {
								bestN, bestG = n, g
							}
						}
						grid = fmt.Sprintf("  grid=%d", bestG)
					}
					log.Printf("[%s · %s] %d buf  %d fr  %dch@%dHz  rms=%.0f  ~%.0f Hz%s  lost=%d",
						st.peer, st.name, st.buffers, st.frames, st.channels, st.sampleRate, rms, freq, grid, st.loss.LostBuffers())
				}
				st.buffers, st.frames, st.sumSq, st.zeroCross = 0, 0, 0, 0
				st.gridFrames = map[int64]int{}
			}
		}
	}
}
