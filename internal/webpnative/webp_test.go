//go:build webpnative && cgo && nodynamic

package webpnative

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"runtime"
	"sync"
	"testing"

	"github.com/gen2brain/webp"
)

// screenshot draws flat panels with text-like strokes and a gradient, which
// PNG keeps losslessly and WebP shrinks. It is the pattern
// cmd/dedup/images_webp_test.go uses, so the sizes compared here are the ones
// the dedup stage meets.
func screenshot(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{uint8(x * 255 / w), uint8(y * 255 / h), uint8((x ^ y) & 0xff), 255}
			if y%20 < 12 && (x*7+y*3)%13 < 5 {
				c = color.RGBA{20, 20, 30, 255}
			}
			img.Set(x, y, c)
		}
	}
	return img
}

// nrgbaAlpha is an image with partial alpha, which exercises the lossy
// alpha plane instead of a constant 255.
func nrgbaAlpha(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{
				R: uint8(x % 256), G: uint8(y % 256), B: uint8((x * y) % 256),
				A: uint8(64 + (x+y)%192),
			})
		}
	}
	return img
}

// paletted is an image type that has to be drawn into an *image.NRGBA before
// encoding; real PNG screenshots are often 8-bit paletted or gray.
func paletted(w, h int) *image.Paletted {
	pal := color.Palette{
		color.RGBA{0, 0, 0, 255}, color.RGBA{255, 255, 255, 255},
		color.RGBA{200, 30, 40, 255}, color.RGBA{30, 200, 40, 128},
	}
	img := image.NewPaletted(image.Rect(0, 0, w, h), pal)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetColorIndex(x, y, uint8((x/7+y/5)%len(pal)))
		}
	}
	return img
}

// gray is another type that needs the NRGBA conversion.
func gray(w, h int) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetGray(x, y, color.Gray{Y: uint8((x*3 + y*5) % 256)})
		}
	}
	return img
}

// pixOf returns an image's rows the way the encoder wants them: *image.RGBA
// and *image.NRGBA hand out their Pix, anything else (a paletted or 16-bit PNG,
// say) is drawn into an *image.NRGBA, exactly as gen2brain/webp does.
func pixOf(img image.Image) (pix []byte, w, h, stride int) {
	b := img.Bounds()
	switch m := img.(type) {
	case *image.RGBA:
		return m.Pix, b.Dx(), b.Dy(), 4 * b.Dx()
	case *image.NRGBA:
		return m.Pix, b.Dx(), b.Dy(), 4 * b.Dx()
	default:
		dst := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
		draw.Draw(dst, dst.Bounds(), img, b.Min, draw.Src)
		return dst.Pix, b.Dx(), b.Dy(), 4 * b.Dx()
	}
}

func encodeImage(t *testing.T, img image.Image, quality, method int) []byte {
	t.Helper()
	pix, w, h, stride := pixOf(img)
	out, err := Encode(pix, w, h, stride, quality, method)
	if err != nil {
		t.Fatalf("Encode(%T %dx%d q%d m%d): %v", img, w, h, quality, method, err)
	}
	return out
}

func TestEncodeProducesAWebP(t *testing.T) {
	for _, tc := range []struct {
		name string
		img  image.Image
	}{
		{"rgba", screenshot(320, 200)},
		{"nrgba-alpha", nrgbaAlpha(320, 200)},
		{"paletted", paletted(320, 200)},
		{"gray", gray(320, 200)},
		{"1x1", screenshot(1, 1)},
		{"3x7", screenshot(3, 7)},
		{"1023x769", screenshot(1023, 769)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := encodeImage(t, tc.img, 85, 2)
			cfg, err := webp.DecodeConfig(bytes.NewReader(out))
			if err != nil {
				t.Fatalf("the output is not a WebP file: %v", err)
			}
			b := tc.img.Bounds()
			if cfg.Width != b.Dx() || cfg.Height != b.Dy() {
				t.Fatalf("got %dx%d, want %dx%d", cfg.Width, cfg.Height, b.Dx(), b.Dy())
			}
		})
	}
}

func TestEncodeIsDeterministic(t *testing.T) {
	img := screenshot(640, 400)
	first := encodeImage(t, img, 85, 2)
	for i := 0; i < 3; i++ {
		if out := encodeImage(t, img, 85, 2); !bytes.Equal(out, first) {
			t.Fatalf("run %d encoded the same pixels to %d bytes, first run to %d",
				i, len(out), len(first))
		}
	}
}

func TestEncodeConcurs(t *testing.T) {
	img := screenshot(512, 384)
	want := encodeImage(t, img, 85, 2)

	// Encode copies its input in C, so several goroutines may share the pixels.
	var wg sync.WaitGroup
	outs := make([][]byte, 2*runtime.GOMAXPROCS(0))
	errs := make([]error, len(outs))
	for i := range outs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pix, w, h, stride := pixOf(img)
			outs[i], errs[i] = Encode(pix, w, h, stride, 85, 2)
		}()
	}
	wg.Wait()
	for i := range outs {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if !bytes.Equal(outs[i], want) {
			t.Fatalf("goroutine %d encoded the same pixels to other bytes", i)
		}
	}
}

func TestEncodeRejectsBadArguments(t *testing.T) {
	pix := screenshot(9, 9).Pix
	for _, tc := range []struct {
		name                  string
		pix                   []byte
		width, height, stride int
		quality, method       int
	}{
		{"zero width", pix, 0, 9, 36, 85, 2},
		{"zero height", pix, 9, 0, 36, 85, 2},
		{"negative width", pix, -9, 9, 36, 85, 2},
		{"negative height", pix, 9, -9, 36, 85, 2},
		{"too wide", pix, 16384, 1, 16384 * 4, 85, 2},
		{"too tall", pix, 1, 16384, 4, 85, 2},
		{"empty pixels", nil, 9, 9, 36, 85, 2},
		{"short pixels", pix[:36*8+35], 9, 9, 36, 85, 2},
		{"stride too small", pix, 9, 9, 35, 85, 2},
		{"quality too high", pix, 9, 9, 36, 101, 2},
		{"quality negative", pix, 9, 9, 36, -1, 2},
		{"method too high", pix, 9, 9, 36, 85, 7},
		{"method negative", pix, 9, 9, 36, 85, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Encode(tc.pix, tc.width, tc.height, tc.stride, tc.quality, tc.method)
			if err == nil {
				t.Fatalf("want an error, got %d bytes", len(out))
			}
			if out != nil {
				t.Fatalf("want no output with the error, got %d bytes", len(out))
			}
		})
	}
}

func TestEncodeAcceptsUnpaddedLastRow(t *testing.T) {
	img := screenshot(9, 9)
	// A stride wider than the row is allowed; the pixels past the last row
	// must not be read.
	stride := 9 * 4
	padded := make([]byte, stride*8+9*4)
	copy(padded, img.Pix)
	if _, err := Encode(padded, 9, 9, stride, 85, 2); err != nil {
		t.Fatalf("exact buffer: %v", err)
	}
	if _, err := Encode(padded[:stride*9], 9, 9, stride, 85, 2); err != nil {
		t.Fatalf("padded stride: %v", err)
	}
}

func TestEncodeKeepsTheInputAlone(t *testing.T) {
	img := screenshot(64, 64)
	pix, w, h, stride := pixOf(img)
	before := bytes.Clone(pix)
	if _, err := Encode(pix, w, h, stride, 85, 2); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pix, before) {
		t.Fatal("Encode changed the pixels it was given")
	}
}

// BenchmarkEncode measures this package on a screenshot-like image at the
// settings the dedup stage uses.
func BenchmarkEncode(b *testing.B) {
	img := screenshot(1440, 900)
	pix, w, h, stride := pixOf(img)
	b.SetBytes(int64(len(pix)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Encode(pix, w, h, stride, 85, 2); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEncodeWasm measures gen2brain/webp's WASM build on the same image,
// for comparison with BenchmarkEncode.
func BenchmarkEncodeWasm(b *testing.B) {
	img := screenshot(1440, 900)
	b.SetBytes(int64(4 * 1440 * 900))
	opts := webp.Options{Quality: 85, Method: 2}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		if err := webp.Encode(&buf, img, opts); err != nil {
			b.Fatal(err)
		}
	}
}
