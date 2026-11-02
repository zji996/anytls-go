//go:build !(dedupimages && nodynamic)

package main

import "errors"

func webpTranscoder(int) (transcodeFunc, error) {
	return nil, errors.New("-images needs a build with -tags dedupimages,nodynamic")
}
