//go:build dedupimages && nodynamic

package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image/png"

	"github.com/gen2brain/webp"
)

// The WebP codec is libwebp transpiled to Go. nodynamic keeps gen2brain/webp
// from loading a system libwebp instead: another version could encode the
// same image to other bytes, and every host must agree byte for byte.

// webpMethod trades encoding effort for size, 0..6. On screenshots method 2
// takes about 40% of the time of method 4 for 3-5% more bytes; from 3 up
// libwebp adds its slow rate-distortion passes.
const webpMethod = 2

// imageMaxPixels bounds the memory one decode may take (4 bytes a pixel).
const imageMaxPixels = 40 << 20

// webpMaxSide is the largest width or height WebP can store.
const webpMaxSide = 16383

var errAnimated = errors.New("animated PNG")

func webpTranscoder(quality int) (transcodeFunc, error) {
	opts := webp.Options{Quality: quality, Method: webpMethod}
	return func(b64 []byte) ([]byte, error) {
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
		img, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		var out bytes.Buffer
		if err := webp.Encode(&out, img, opts); err != nil {
			return nil, err
		}
		return base64.StdEncoding.AppendEncode(nil, out.Bytes()), nil
	}, nil
}

// animatedPNG reports whether an acTL chunk comes before the image data: a
// still re-encode would keep only the first frame.
func animatedPNG(raw []byte) bool {
	idat := bytes.Index(raw, []byte("IDAT"))
	actl := bytes.Index(raw, []byte("acTL"))
	return actl >= 0 && (idat < 0 || actl < idat)
}
