//go:build !(dedupimages && (nodynamic || (webpnative && cgo)))

package main

import "errors"

func webpTranscoder(int) (transcodeFunc, error) {
	return nil, errors.New("-images needs a build with -tags dedupimages,nodynamic " +
		"(libwebp transpiled to Go) or -tags dedupimages,webpnative and CGO_ENABLED=1 " +
		"(native libwebp)")
}
