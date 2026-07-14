// gen-sweep writes a test WAV containing a continuously rising (logarithmic)
// sine sweep. Feeding this through WAIL (`wail --headless --wav <file>`) makes
// received-audio integrity easy to judge: the pitch should climb smoothly, so
// any dropout, gap, or out-of-order interval is immediately audible (and the
// linkaudio-probe's frequency estimate should ramp monotonically).
package main

import (
	"encoding/binary"
	"flag"
	"log"
	"math"
	"os"
)

func main() {
	out := flag.String("o", "/tmp/wail-sweep.wav", "output WAV path")
	dur := flag.Float64("dur", 120, "duration in seconds")
	f0 := flag.Float64("f0", 80, "start frequency (Hz)")
	f1 := flag.Float64("f1", 12000, "end frequency (Hz)")
	rate := flag.Int("rate", 48000, "sample rate")
	amp := flag.Float64("amp", 0.5, "amplitude 0..1")
	flag.Parse()

	const channels = 2
	const bits = 16
	sr := *rate
	nFrames := int(*dur * float64(sr))
	if nFrames <= 0 || *f0 <= 0 || *f1 <= 0 {
		log.Fatal("invalid parameters")
	}

	f, err := os.Create(*out)
	if err != nil {
		log.Fatalf("create: %v", err)
	}
	defer f.Close()

	dataBytes := nFrames * channels * (bits / 8)
	// Canonical 44-byte PCM WAV header.
	hdr := make([]byte, 44)
	copy(hdr[0:4], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(36+dataBytes))
	copy(hdr[8:12], "WAVE")
	copy(hdr[12:16], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:20], 16)   // fmt chunk size
	binary.LittleEndian.PutUint16(hdr[20:22], 1)    // PCM
	binary.LittleEndian.PutUint16(hdr[22:24], channels)
	binary.LittleEndian.PutUint32(hdr[24:28], uint32(sr))
	binary.LittleEndian.PutUint32(hdr[28:32], uint32(sr*channels*(bits/8))) // byte rate
	binary.LittleEndian.PutUint16(hdr[32:34], channels*(bits/8))            // block align
	binary.LittleEndian.PutUint16(hdr[34:36], bits)
	copy(hdr[36:40], "data")
	binary.LittleEndian.PutUint32(hdr[40:44], uint32(dataBytes))
	if _, err := f.Write(hdr); err != nil {
		log.Fatalf("write header: %v", err)
	}

	// Logarithmic sweep: f(t) = f0 * (f1/f0)^(t/T). Phase is its integral, which
	// keeps the waveform continuous (no clicks) as the frequency rises.
	k := math.Log(*f1 / *f0)
	peak := *amp * 32767.0
	buf := make([]byte, 0, 65536)
	flush := func() { f.Write(buf); buf = buf[:0] }
	for i := 0; i < nFrames; i++ {
		t := float64(i) / float64(sr)
		phase := 2 * math.Pi * (*f0) * (*dur) / k * (math.Exp(k*t/(*dur)) - 1)
		v := int16(peak * math.Sin(phase))
		var s [2]byte
		binary.LittleEndian.PutUint16(s[:], uint16(v))
		buf = append(buf, s[0], s[1], s[0], s[1]) // same sample in L and R
		if len(buf) >= 65536 {
			flush()
		}
	}
	flush()

	log.Printf("wrote %s: %.0fs, %d Hz→%d Hz log sweep, %d-ch %d-bit @ %d Hz (%d MB)",
		*out, *dur, int(*f0), int(*f1), channels, bits, sr, dataBytes/(1024*1024))
}
