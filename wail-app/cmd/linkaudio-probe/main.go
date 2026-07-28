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
	"sort"
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

	// offset analysis (when -offset-ref is set): ring of per-buffer RMS
	// envelopes for cross-correlation against the reference channel.
	rmsRing  []float64
	tempoBPM float64
}

func main() {
	peerName := flag.String("name", "wail-probe", "Link Audio peer name for this probe")
	match := flag.String("match", "", "only subscribe to channels whose \"peer · name\" contains this substring (case-insensitive); empty = all")
	bpi := flag.Float64("bpi", 0, "if >0, also report the shared-grid interval index of received audio (beat mapped at this beats-per-interval lens; must match the room's BPI)")
	offsetRef := flag.String("offset-ref", "", "if set, report per-channel onset phase offset in ms vs the channel whose name contains this substring (e.g. \"Metronome\")")
	offsetDump := flag.String("offset-dump", "", "if set (and -offset-ref), write ref+channel RMS envelopes as CSVs to this directory on exit")
	flag.Parse()
	matchLower := strings.ToLower(*match)
	offsetRefLower := strings.ToLower(*offsetRef)

	l := abllink.New(120)
	l.SetPeerName(*peerName)
	l.Enable(true)
	l.EnableLinkAudio(true)
	defer l.Close()

	var ss *abllink.SessionState
	if *bpi > 0 || *offsetRef != "" {
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
			if *offsetDump != "" {
				dumpEnvelopes(subs, *offsetDump)
			}
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

					// Offset analysis: keep the per-buffer RMS envelope ring
					// for cross-correlation (see reportOffsets).
					if *offsetRef != "" && buf.NumFrames > 0 {
						st.tempoBPM = buf.TempoBPM
						st.rmsRing = appendPhase(st.rmsRing, chanBufRMS(buf), 4096)
					}
				}
			}

			if !report {
				continue
			}

			// Offset analysis report (every 15s): per-channel onset phase vs the
			// reference channel's (e.g. the WAIL Metronome, grid-rendered).
			if *offsetRef != "" && tick%(reportEvery*15) == 0 {
				reportOffsets(subs, offsetRefLower)
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

// --- Offset analysis helpers ---

// chanBufRMS computes the RMS of one buffer's channel 0.
func chanBufRMS(buf abllink.CaptureBuffer) float64 {
	ch := buf.NumChannels
	if ch < 1 {
		ch = 1
	}
	if buf.NumFrames == 0 {
		return 0
	}
	var s float64
	for i := 0; i < buf.NumFrames; i++ {
		idx := i * ch
		if idx >= len(buf.Samples) {
			break
		}
		v := float64(buf.Samples[idx])
		s += v * v
	}
	return math.Sqrt(s / float64(buf.NumFrames))
}

// appendPhase appends v to a bounded ring.
func appendPhase(hist []float64, v float64, max int) []float64 {
	hist = append(hist, v)
	if len(hist) > max {
		hist = hist[len(hist)-max:]
	}
	return hist
}

func medianHist(h []float64) float64 {
	if len(h) == 0 {
		return 0
	}
	cp := make([]float64, len(h))
	copy(cp, h)
	sort.Float64s(cp)
	return cp[len(cp)/2]
}

// reportOffsets cross-correlates each channel's RMS envelope against the
// reference channel's (the grid-rendered metronome) and reports the peak lag
// in milliseconds. Pattern-agnostic and robust: any periodic content (clicks,
// patterns) locks to its phase offset; fixed DAW-chain latency shows directly
// while grid/network effects cancel through the shared path.
func reportOffsets(subs map[abllink.ChannelID]*chanState, refLower string) {
	var ref *chanState
	for _, st := range subs {
		if strings.Contains(strings.ToLower(st.peer+" · "+st.name), refLower) {
			ref = st
			break
		}
	}
	if ref == nil || len(ref.rmsRing) < 400 {
		return
	}
	for _, st := range subs {
		if st == ref || len(st.rmsRing) < 400 {
			continue
		}
		lagMs, strength := xcorrPeak(ref.rmsRing, st.rmsRing)
		top := xcorrTop(ref.rmsRing, st.rmsRing, 3)
		log.Printf("[offset] %q · %q: %+.0f ms vs %q · %q (peak %.2f; top3 %v)",
			st.peer, st.name, lagMs, ref.peer, ref.name, strength, top)
	}
}

// xcorrTop returns the top-n (lagMs, strength) correlation peaks for
// inspecting alias structure.
func xcorrTop(a, b []float64, nTop int) [][2]float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n > 2000 {
		n = 2000
	}
	a = a[len(a)-n:]
	b = b[len(b)-n:]
	var ma, mb float64
	for i := 0; i < n; i++ {
		ma += a[i]
		mb += b[i]
	}
	ma /= float64(n)
	mb /= float64(n)
	const bufMs = 128.0 / 48.0
	maxLag := int(math.Round(250.0 / bufMs))
	type peak struct{ s, lag float64 }
	var peaks []peak
	for lag := -maxLag; lag <= maxLag; lag++ {
		var sum float64
		for i := 0; i < n; i++ {
			j := i + lag
			if j < 0 || j >= n {
				continue
			}
			sum += (a[i] - ma) * (b[j] - mb)
		}
		peaks = append(peaks, peak{sum, float64(lag)})
	}
	sort.Slice(peaks, func(i, j int) bool { return peaks[i].s > peaks[j].s })
	out := make([][2]float64, 0, nTop)
	for i := 0; i < nTop && i < len(peaks); i++ {
		out = append(out, [2]float64{peaks[i].lag * bufMs, peaks[i].s})
	}
	return out
}

// xcorrPeak returns the lag (in ms) at which the cross-correlation of
// envelopes a (reference) and b peaks, plus a rough normalized strength.
// Positive lag = b is LATE relative to a. Envelopes are per received Link
// Audio buffer (~128 frames @ 48kHz = 2.667ms steps).
func xcorrPeak(a, b []float64) (float64, float64) {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n > 2000 {
		n = 2000
	}
	a = a[len(a)-n:]
	b = b[len(b)-n:]
	var ma, mb float64
	for i := 0; i < n; i++ {
		ma += a[i]
		mb += b[i]
	}
	ma /= float64(n)
	mb /= float64(n)
	const bufMs = 128.0 / 48.0
	maxLag := int(math.Round(250.0 / bufMs))
	best, bestLag, zero := 0.0, 0, 0.0
	for lag := -maxLag; lag <= maxLag; lag++ {
		var sum float64
		for i := 0; i < n; i++ {
			j := i + lag
			if j < 0 || j >= n {
				continue
			}
			sum += (a[i] - ma) * (b[j] - mb)
		}
		if lag == 0 {
			zero = sum
		}
		if sum > best {
			best, bestLag = sum, lag
		}
	}
	strength := 0.0
	if zero != 0 {
		strength = best / math.Abs(zero)
	}
	return float64(bestLag) * bufMs, strength
}

// dumpEnvelopes writes each channel's RMS envelope ring to <dir>/<peer>-<name>.csv
// (one value per line, ~2.6ms per step) for offline cross-correlation.
func dumpEnvelopes(subs map[abllink.ChannelID]*chanState, dir string) {
	for _, st := range subs {
		if len(st.rmsRing) == 0 {
			continue
		}
		name := fmt.Sprintf("%s/%s-%s.csv", dir, sanitizeName(st.peer), sanitizeName(st.name))
		f, err := os.Create(name)
		if err != nil {
			log.Printf("[offset-dump] %v", err)
			continue
		}
		for _, v := range st.rmsRing {
			fmt.Fprintf(f, "%.1f\n", v)
		}
		f.Close()
		log.Printf("[offset-dump] wrote %s (%d samples)", name, len(st.rmsRing))
	}
}

func sanitizeName(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			out = append(out, r)
		} else {
			out = append(out, '_')
		}
	}
	return string(out)
}
