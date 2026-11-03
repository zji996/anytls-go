//go:build dedupimages && webpnative && cgo

package main

import (
	"encoding/base64"

	"anytls/internal/webpnative"
)

// The WebP codec is libwebp v1.6.0 compiled into the binary from the C source
// vendored in internal/webpnative. It encodes to the same bytes as the WASM
// build in images_webp.go - both run the same C code with SIMD and threads
// off - but several times faster, and a static binary needs no system libwebp.
// Build with -tags dedupimages,nodynamic,webpnative and CGO_ENABLED=1;
// nodynamic keeps the tests' gen2brain/webp reference on its WASM build.

func webpTranscoder(quality int) (transcodeFunc, error) {
	return func(b64 []byte) ([]byte, error) {
		img, err := decodePNGDataURL(b64)
		if err != nil {
			return nil, err
		}
		pix, width, height, stride := imagePix(img)
		out, err := webpnative.Encode(pix, width, height, stride, quality, webpMethod)
		if err != nil {
			return nil, err
		}
		return base64.StdEncoding.AppendEncode(nil, out), nil
	}, nil
}
