package main

import (
	"bytes"
	"testing"
)

func TestFrameShrinksNewBase64(t *testing.T) {
	body := []byte(image(400 << 10))
	res, err := encodeFrame(body, newLRU(64<<20))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.frame)*10 > len(body)*8 {
		t.Fatalf("frame %d bytes for %d bytes of base64", len(res.frame), len(body))
	}
	got, missing, err := decodeFrame(res.frame, len(body), newLRU(64<<20))
	if err != nil || len(missing) != 0 || !bytes.Equal(got, body) {
		t.Fatalf("round trip: err %v, missing %d, equal %v", err, len(missing), bytes.Equal(got, body))
	}
}
