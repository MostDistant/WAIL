// gen-complex writes a test WAV of dense, music-like program material
// (detuned pad, stepping bass, percussive transients, noise floor — see
// internal/dsp.GenerateComplexProgram). Where gen-sweep makes ordering
// problems audible, this stresses the codec path the way real instruments
// do: rich spectra, sharp transients, stereo decorrelation.
package main

import (
	"encoding/binary"
	"flag"
	"log"
	"os"

	"github.com/nicholasgasior/wail/wail-app/internal/dsp"
)

func main() {
	out := flag.String("o", "/tmp/wail-complex.wav", "output WAV path")
	dur := flag.Float64("dur", 60, "duration in seconds")
	rate := flag.Int("rate", 48000, "sample rate")
	flag.Parse()

	const channels = 2
	const bits = 16
	nFrames := int(*dur * float64(*rate))
	samples := dsp.GenerateComplexProgram(nFrames, *rate)
	if samples == nil {
		log.Fatal("invalid parameters")
	}

	f, err := os.Create(*out)
	if err != nil {
		log.Fatalf("create: %v", err)
	}
	defer f.Close()

	dataBytes := len(samples) * 2
	hdr := make([]byte, 44)
	copy(hdr[0:4], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(36+dataBytes))
	copy(hdr[8:12], "WAVE")
	copy(hdr[12:16], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:20], 16)
	binary.LittleEndian.PutUint16(hdr[20:22], 1)
	binary.LittleEndian.PutUint16(hdr[22:24], channels)
	binary.LittleEndian.PutUint32(hdr[24:28], uint32(*rate))
	binary.LittleEndian.PutUint32(hdr[28:32], uint32(*rate*channels*(bits/8)))
	binary.LittleEndian.PutUint16(hdr[32:34], channels*(bits/8))
	binary.LittleEndian.PutUint16(hdr[34:36], bits)
	copy(hdr[36:40], "data")
	binary.LittleEndian.PutUint32(hdr[40:44], uint32(dataBytes))
	if _, err := f.Write(hdr); err != nil {
		log.Fatalf("write header: %v", err)
	}

	buf := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(s))
	}
	if _, err := f.Write(buf); err != nil {
		log.Fatalf("write samples: %v", err)
	}
	log.Printf("wrote %s: %.0fs stereo complex program at %d Hz", *out, *dur, *rate)
}
