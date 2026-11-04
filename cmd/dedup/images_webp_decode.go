//go:build dedupimages && (nodynamic || (webpnative && cgo))

package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	stdimage "image"
	"image/draw"
	"image/png"
)

// The request-body side of the WebP codec, shared by the two encoder builds:
// -tags dedupimages,nodynamic uses libwebp transpiled to Go, -tags
// dedupimages,webpnative uses libwebp compiled into the binary. Both must
// accept and reject the same inputs and hand the encoder the same pixels, so
// that a given PNG becomes the same WebP whichever build a host runs.

// webpMethod trades encoding effort for size, 0..6. On screenshots method 2
// takes about 40% of the time of method 4 for 3-5% more bytes; from 3 up
// libwebp adds its slow rate-distortion passes.
const webpMethod = 2

// imageMaxPixels bounds the pixel count accepted by the codec.
const imageMaxPixels = 40 << 20

// webpMaxSide is the largest width or height WebP can store.
const webpMaxSide = 16383

var errAnimated = errors.New("animated PNG")

// decodePNGDataURL turns the base64 payload of a PNG data URL into the decoded
// image.
func decodePNGDataURL(b64 []byte) (stdimage.Image, error) {
	w, h, err := pngHeaderSize(b64)
	if err != nil {
		return nil, err
	}
	if w > webpMaxSide || h > webpMaxSide || w*h > imageMaxPixels {
		return nil, fmt.Errorf("image %dx%d too large", w, h)
	}
	raw := make([]byte, base64.StdEncoding.DecodedLen(len(b64)))
	n, err := base64.StdEncoding.Decode(raw, b64)
	if err != nil {
		return nil, err
	}
	raw = raw[:n]
	if animatedPNG(raw) {
		return nil, errAnimated
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if cfg.Width > webpMaxSide || cfg.Height > webpMaxSide || cfg.Width*cfg.Height > imageMaxPixels {
		return nil, fmt.Errorf("image %dx%d too large", cfg.Width, cfg.Height)
	}
	return png.Decode(bytes.NewReader(raw))
}

// imagePix returns an image's RGBA rows and their row stride: *image.RGBA and
// *image.NRGBA hand out their Pix untouched, anything else is drawn into an
// *image.NRGBA, which is exactly what gen2brain/webp does before encoding.
func imagePix(img stdimage.Image) (pix []byte, width, height, stride int) {
	b := img.Bounds()
	switch m := img.(type) {
	case *stdimage.RGBA:
		return m.Pix, b.Dx(), b.Dy(), 4 * b.Dx()
	case *stdimage.NRGBA:
		return m.Pix, b.Dx(), b.Dy(), 4 * b.Dx()
	default:
		dst := stdimage.NewNRGBA(stdimage.Rect(0, 0, b.Dx(), b.Dy()))
		draw.Draw(dst, dst.Bounds(), img, b.Min, draw.Src)
		return dst.Pix, b.Dx(), b.Dy(), 4 * b.Dx()
	}
}

// animatedPNG reports whether an acTL chunk comes before the image data: a
// still re-encode would keep only the first frame.
func animatedPNG(raw []byte) bool {
	idat := bytes.Index(raw, []byte("IDAT"))
	actl := bytes.Index(raw, []byte("acTL"))
	return actl >= 0 && (idat < 0 || actl < idat)
}
