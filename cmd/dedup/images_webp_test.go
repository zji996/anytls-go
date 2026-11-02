//go:build dedupimages && nodynamic

package main

import (
	"bytes"
	"encoding/base64"
	stdimage "image"
	"image/color"
	stdpng "image/png"
	"sync"
	"testing"

	"github.com/gen2brain/webp"
)

// screenshot draws flat panels with text-like strokes and a gradient, which
// PNG keeps losslessly and WebP shrinks.
func screenshot(w, h int) []byte {
	img := stdimage.NewRGBA(stdimage.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{uint8(x * 255 / w), uint8(y * 255 / h), uint8((x ^ y) & 0xff), 255}
			if y%20 < 12 && (x*7+y*3)%13 < 5 {
				c = color.RGBA{20, 20, 30, 255}
			}
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	stdpng.Encode(&b, img)
	return b.Bytes()
}

func TestWebpTranscoderIsDeterministicAndSmaller(t *testing.T) {
	transcode, err := webpTranscoder(90)
	if err != nil {
		t.Fatal(err)
	}
	in := base64.StdEncoding.AppendEncode(nil, screenshot(640, 400))
	a, err := transcode(in)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := transcode(bytes.Clone(in))
	if !bytes.Equal(a, b) {
		t.Fatal("the same PNG gave two different WebPs")
	}
	if len(a) >= len(in) {
		t.Fatalf("WebP %d bytes is not smaller than PNG %d", len(a), len(in))
	}
	raw, err := base64.StdEncoding.DecodeString(string(a))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := webp.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width != 640 || cfg.Height != 400 {
		t.Fatalf("output is not a 640x400 WebP: %+v %v", cfg, err)
	}
}

func TestWebpTranscoderAgreesWithItselfAcrossWorkers(t *testing.T) {
	transcode, _ := webpTranscoder(85)
	in := base64.StdEncoding.AppendEncode(nil, screenshot(800, 500))
	want, err := transcode(in)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	outs := make([][]byte, 4)
	for i := range outs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], _ = transcode(in)
		}()
	}
	wg.Wait()
	for i, out := range outs {
		if !bytes.Equal(out, want) {
			t.Fatalf("worker %d encoded the same PNG to other bytes", i)
		}
	}
}

func TestWebpTranscoderRejectsWhatItMustNotTouch(t *testing.T) {
	transcode, _ := webpTranscoder(90)
	png := screenshot(64, 64)
	// An acTL chunk before IDAT marks an animated PNG.
	animated := bytes.Replace(png, []byte("IDAT"), []byte("acTLIDAT"), 1)
	cases := map[string][]byte{
		"not base64": []byte("@@@@"),
		"not a PNG":  base64.StdEncoding.AppendEncode(nil, []byte("GIF89a not a png at all")),
		"animated":   base64.StdEncoding.AppendEncode(nil, animated),
	}
	for name, in := range cases {
		if _, err := transcode(in); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
