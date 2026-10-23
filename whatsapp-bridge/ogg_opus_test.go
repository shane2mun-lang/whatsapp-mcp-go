package main

import (
	"encoding/binary"
	"math"
	"testing"
)

// oggPage builds one Ogg page holding a single packet. The analyzer never
// checks the CRC, so it is left zero.
func oggPage(serial, seq uint32, granule uint64, packet []byte) []byte {
	var segs []byte
	n := len(packet)
	for n >= 255 {
		segs = append(segs, 255)
		n -= 255
	}
	segs = append(segs, byte(n))
	page := make([]byte, 27, 27+len(segs)+len(packet))
	copy(page, "OggS")
	binary.LittleEndian.PutUint64(page[6:14], granule)
	binary.LittleEndian.PutUint32(page[14:18], serial)
	binary.LittleEndian.PutUint32(page[18:22], seq)
	page[26] = byte(len(segs))
	page = append(page, segs...)
	return append(page, packet...)
}

// opusStream is one logical stream: OpusHead, OpusTags, then audio pages
// ending at finalGranule. inputRate goes into OpusHead to prove it is ignored.
func opusStream(serial uint32, preSkip uint16, inputRate uint32, finalGranule uint64) []byte {
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8], head[9] = 1, 1
	binary.LittleEndian.PutUint16(head[10:12], preSkip)
	binary.LittleEndian.PutUint32(head[12:16], inputRate)

	out := oggPage(serial, 0, 0, head)
	out = append(out, oggPage(serial, 1, 0, []byte("OpusTags\x00\x00\x00\x00\x00\x00\x00\x00"))...)
	out = append(out, oggPage(serial, 2, finalGranule/2, make([]byte, 300))...)
	return append(out, oggPage(serial, 3, finalGranule, make([]byte, 300))...)
}

func TestAnalyzeOggOpusDuration(t *testing.T) {
	const rate = 48000
	cases := []struct {
		name string
		data []byte
		want uint32
	}{
		{"single stream", opusStream(1, 312, 48000, 15*rate+312), 15},
		{"16 kHz input still counts 48 kHz granules", opusStream(1, 312, 16000, 15*rate+312), 15},
		{"chained TTS chunks are summed", append(append(
			opusStream(1, 312, 48000, 7*rate+312),
			opusStream(2, 312, 48000, 7*rate+312)...),
			opusStream(3, 312, 48000, rate+312)...), 15},
		{"trailing page without a finished packet", append(
			opusStream(1, 312, 48000, 15*rate+312),
			oggPage(1, 4, math.MaxUint64, make([]byte, 100))...), 15},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := analyzeOggOpus(tc.data)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("duration = %d, want %d", got, tc.want)
			}
		})
	}
}
