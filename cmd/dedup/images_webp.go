//go:build dedupimages && nodynamic && !(webpnative && cgo)

package main

import (
	"bytes"
	"encoding/base64"

	"github.com/gen2brain/webp"
)

// The WebP codec is libwebp transpiled to Go. nodynamic keeps gen2brain/webp
// from loading a system libwebp instead: another version could encode the
// same image to other bytes, and every host must agree byte for byte.

// Only the native codec holds part of the decode budget outside Go.
const imageDecodeGoMiB = imageDecodeBudgetMiB

func webpTranscoder(quality int) (transcodeFunc, error) {
	opts := webp.Options{Quality: quality, Method: webpMethod}
	return func(b64 []byte) ([]byte, error) {
		img, err := decodePNGDataURL(b64)
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
