//go:build webpnative && cgo && nodynamic

package webpnative

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gen2brain/webp"
)

// The native build and gen2brain/webp's WASM build run the same libwebp
// v1.6.0 with SIMD and threads off, so they must produce the same file for the
// same pixels, quality and method. Anything else - a stray SIMD path, a
// different config.h default, different compiler flags - would make a host
// that switched builds re-encode its screenshots, and an upstream prompt cache
// matches resent images byte for byte.

// wasmEncode encodes through gen2brain/webp's WASM build the way its
// Encode() does after imageToNRGBA: *image.RGBA and *image.NRGBA hand over
// their Pix, everything else is drawn into an *image.NRGBA first.
func wasmEncode(img image.Image, quality, method int) ([]byte, error) {
	var buf bytes.Buffer
	opts := webp.Options{Quality: quality, Method: method}
	if err := webp.Encode(&buf, img, opts); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func nativeEncode(img image.Image, quality, method int) ([]byte, error) {
	pix, w, h, stride := pixOf(img)
	return Encode(pix, w, h, stride, quality, method)
}

// firstDiff describes where two WebP files start to differ, which is more use
// than two hashes.
func firstDiff(a, b []byte) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return fmt.Sprintf("sizes %d and %d, first difference at byte %d: %02x != %02x",
				len(a), len(b), i, a[i], b[i])
		}
	}
	return fmt.Sprintf("sizes %d and %d, identical prefix", len(a), len(b))
}

// testImages is the set every comparison runs on: the dedup tests' synthetic
// screenshot, one with partial alpha, and the odd sizes that catch a stride or
// rounding mistake.
func testImages(t *testing.T) map[string]image.Image {
	t.Helper()
	return map[string]image.Image{
		"screenshot-640x400":  screenshot(640, 400),
		"screenshot-1440x900": screenshot(1440, 900),
		"nrgba-alpha-320x200": nrgbaAlpha(320, 200),
		"nrgba-alpha-511x257": nrgbaAlpha(511, 257),
		"paletted-160x120":    paletted(160, 120),
		"gray-97x61":          gray(97, 61),
		"1x1":                 screenshot(1, 1),
		"3x7":                 screenshot(3, 7),
		"1023x769":            screenshot(1023, 769),
	}
}

// pngDirImages reads every PNG in $PNGDIR, if it is set. Real screenshots are
// far more varied than the synthetic ones; the test passes without them.
func pngDirImages(tb testing.TB) map[string]image.Image {
	tb.Helper()
	dir := os.Getenv("PNGDIR")
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		tb.Fatalf("PNGDIR=%s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".png") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	images := make(map[string]image.Image, len(names))
	for _, name := range names {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			tb.Fatalf("%s: %v", name, err)
		}
		img, err := png.Decode(f)
		f.Close()
		if err != nil {
			tb.Fatalf("%s: %v", name, err)
		}
		images["pngdir/"+name] = img
	}
	if len(images) == 0 {
		tb.Logf("PNGDIR=%s holds no PNG files", dir)
	}
	return images
}

func TestEncodeMatchesWasm(t *testing.T) {
	images := testImages(t)
	for name, img := range pngDirImages(t) {
		images[name] = img
	}
	settings := []struct{ quality, method int }{
		{85, 2}, // what cmd/dedup uses
		{75, 0},
		{90, 4},
	}
	names := make([]string, 0, len(images))
	for name := range images {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		img := images[name]
		for _, s := range settings {
			t.Run(fmt.Sprintf("%s/q%d-m%d", name, s.quality, s.method), func(t *testing.T) {
				want, err := wasmEncode(img, s.quality, s.method)
				if err != nil {
					t.Fatalf("wasm encode: %v", err)
				}
				got, err := nativeEncode(img, s.quality, s.method)
				if err != nil {
					t.Fatalf("native encode: %v", err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("native and wasm differ (%s %dx%d): %s",
						name, img.Bounds().Dx(), img.Bounds().Dy(), firstDiff(got, want))
				}
			})
		}
	}
}

// BenchmarkEncodePNGDir compares the two builds on real screenshots: every PNG
// in $PNGDIR, at the settings cmd/dedup uses. Without $PNGDIR there is nothing
// to measure.
func BenchmarkEncodePNGDir(b *testing.B) {
	images := pngDirImages(b)
	if len(images) == 0 {
		b.Skip("PNGDIR is not set")
	}
	names := make([]string, 0, len(images))
	for name := range images {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		img := images[name]
		b.Run(name+"/native", func(b *testing.B) {
			b.SetBytes(int64(4 * img.Bounds().Dx() * img.Bounds().Dy()))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := nativeEncode(img, 85, 2); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(name+"/wasm", func(b *testing.B) {
			b.SetBytes(int64(4 * img.Bounds().Dx() * img.Bounds().Dy()))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := wasmEncode(img, 85, 2); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
