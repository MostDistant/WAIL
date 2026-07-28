// linktempo is a Link session tempo actor for E2E diagnosis: it joins the LAN
// Link session as a peer and commits tempo changes on a script, the way a DAW
// user turning the tempo knob would (a committed timeline — newest wins).
//
// Usage:
//
//	linktempo -bpm 120 -script "10:122,20:124,30:120"   # t-seconds:bpm steps
//	linktempo -bpm 122 -insist-ms 500                    # re-apply -bpm every 500ms
//
// -insist-ms models a LAN peer that refuses to follow (an external session or
// a DAW whose user keeps turning the knob back): after every script step (or
// from t=0 with no script) it re-commits its target tempo on the interval.
package main

import (
	"flag"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/nicholasgasior/wail/wail-app/internal/abllink"
)

func main() {
	bpm := flag.Float64("bpm", 120, "initial/target tempo")
	name := flag.String("name", "linktempo", "Link peer name")
	script := flag.String("script", "", "comma-separated t-seconds:bpm steps, e.g. \"10:122,20:124\"")
	insistMs := flag.Int("insist-ms", 0, "if >0, re-commit the current target tempo every N ms")
	dur := flag.Float64("dur", 0, "exit after N seconds (0 = run until killed)")
	flag.Parse()

	type step struct {
		at  time.Duration
		bpm float64
	}
	var steps []step
	if *script != "" {
		for _, part := range strings.Split(*script, ",") {
			kv := strings.SplitN(part, ":", 2)
			if len(kv) != 2 {
				log.Fatalf("bad script step %q (want t:bpm)", part)
			}
			sec, err1 := strconv.ParseFloat(kv[0], 64)
			b, err2 := strconv.ParseFloat(kv[1], 64)
			if err1 != nil || err2 != nil || b <= 0 {
				log.Fatalf("bad script step %q", part)
			}
			steps = append(steps, step{at: time.Duration(sec * float64(time.Second)), bpm: b})
		}
	}

	l := abllink.New(*bpm)
	l.SetPeerName(*name)
	l.Enable(true)
	defer l.Close()
	ss := abllink.NewSessionState()

	setTempo := func(b float64, why string) {
		t := l.ClockMicros()
		l.CaptureAppSessionState(ss)
		ss.SetTempo(b, t)
		l.CommitAppSessionState(ss)
		log.Printf("[linktempo] committed tempo %.2f (%s)", b, why)
	}

	target := *bpm
	start := time.Now()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	stepIdx := 0
	lastInsist := time.Time{}
	log.Printf("[linktempo] joined as %q at %.1f BPM, script=%d steps, insist=%dms", *name, *bpm, len(steps), *insistMs)

	for {
		<-tick.C
		el := time.Since(start)
		if *dur > 0 && el >= time.Duration(*dur*float64(time.Second)) {
			return
		}
		if stepIdx < len(steps) && el >= steps[stepIdx].at {
			target = steps[stepIdx].bpm
			setTempo(target, fmt.Sprintf("script step %d", stepIdx))
			stepIdx++
		}
		if *insistMs > 0 && time.Since(lastInsist) >= time.Duration(*insistMs)*time.Millisecond {
			l.CaptureAppSessionState(ss)
			if cur := ss.Tempo(); cur != target {
				setTempo(target, fmt.Sprintf("insist (session was %.2f)", cur))
			}
			lastInsist = time.Now()
		}
	}
}
