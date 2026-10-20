package main

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// prose is compressible text standing in for tool output and messages.
func prose(n int) []byte {
	words := strings.Fields("the model returned a diff for main.go with tests and a summary of the failing check " +
		"func return err nil if for range string int byte context request response stream token")
	r := rand.New(rand.NewPCG(1, 2))
	var b bytes.Buffer
	for b.Len() < n {
		b.WriteString(words[r.IntN(len(words))])
		b.WriteByte(' ')
	}
	return b.Bytes()[:n]
}

var modes = []struct {
	name string
	mode compression
}{{"auto", compressAuto}, {"huffman", compressHuffman}, {"off", compressOff}}

// BenchmarkEncode frames 3 MiB in each mode: new base64, new text, and
// content the decoder already holds. The last is the steady state of a
// conversation resending its history.
func BenchmarkEncode(b *testing.B) {
	inputs := []struct {
		name string
		body []byte
	}{{"new-base64", []byte(image(3 << 20))}, {"new-text", prose(4 << 20)}}
	for _, in := range inputs {
		for _, m := range modes {
			b.Run(in.name+"/"+m.name, func(b *testing.B) {
				b.SetBytes(int64(len(in.body)))
				var frame int
				for b.Loop() {
					res, err := encodeFrame(in.body, newLRU(64<<20), m.mode)
					if err != nil {
						b.Fatal(err)
					}
					frame = len(res.frame)
				}
				b.ReportMetric(float64(len(in.body))/float64(frame), "ratio")
			})
		}
	}
	b.Run("cached", func(b *testing.B) {
		body := prose(4 << 20)
		known := newLRU(64 << 20)
		encodeFrame(body, known, compressAuto)
		b.SetBytes(int64(len(body)))
		for b.Loop() {
			encodeFrame(body, known, compressAuto)
		}
	})
}

func TestFrameModesRoundTrip(t *testing.T) {
	inputs := map[string][]byte{"base64": []byte(image(400 << 10)), "text": prose(400 << 10)}
	for name, body := range inputs {
		for _, m := range modes {
			res, err := encodeFrame(body, newLRU(64<<20), m.mode)
			if err != nil {
				t.Fatal(err)
			}
			got, missing, err := decodeFrame(res.frame, len(body), newLRU(64<<20))
			if err != nil || len(missing) != 0 || !bytes.Equal(got, body) {
				t.Fatalf("%s/%s: err %v, missing %d", name, m.name, err, len(missing))
			}
			ratio := float64(len(body)) / float64(len(res.frame))
			if m.mode != compressOff && ratio < 1.25 {
				t.Errorf("%s/%s: ratio %.2f", name, m.name, ratio)
			}
			if m.mode == compressOff && ratio > 1.01 {
				t.Errorf("%s/off compressed: ratio %.2f", name, ratio)
			}
		}
	}
}

func TestParseCompression(t *testing.T) {
	for _, s := range []string{"auto", "huffman", "off"} {
		if _, err := parseCompression(s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := parseCompression("best"); err == nil {
		t.Fatal(fmt.Errorf("accepted an unknown mode"))
	}
}
