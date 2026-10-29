package main

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// vectorKey, vectorPayload and vectorNow are the fixed token vector shared with
// the Gateway side for cross-checking; vectorToken is what both must produce.
var (
	vectorKey = mustHex("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	vectorNow = time.Unix(1700000000, 0).UTC()
	vectorPay = tokenPayload{
		V:    1,
		Obj:  "0123456789abcdef0123456789abcdef",
		Loc:  "AAAA",
		Exp:  1700000300,
		Size: 12345,
		Type: "application/pdf",
		Disp: `inline; filename="a.pdf"`,
	}
	vectorToken = "eyJ2IjoxLCJvYmoiOiIwMTIzNDU2Nzg5YWJjZGVmMDEyMzQ1Njc4OWFiY2RlZiIsImxvYyI6IkFBQUEiLCJleHAiOjE3MDAwMDAzMDAsInNpemUiOjEyMzQ1LCJ0eXBlIjoiYXBwbGljYXRpb24vcGRmIiwiZGlzcCI6ImlubGluZTsgZmlsZW5hbWU9XCJhLnBkZlwiIn0" +
		".bjKwTKNa6BHKEttpyQKZNT-f-4IP3UXboFzWtK-Po5c"
)

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func TestTokenFixedVector(t *testing.T) {
	got, err := signToken(vectorKey, vectorPay)
	if err != nil {
		t.Fatal(err)
	}
	if got != vectorToken {
		t.Fatalf("signed token differs from the shared vector:\n got %s\nwant %s", got, vectorToken)
	}
	pay, err := parseToken(vectorToken, vectorKey, vectorNow)
	if err != nil {
		t.Fatalf("vector token did not verify: %v", err)
	}
	if *pay != vectorPay {
		t.Fatalf("payload round trip: got %+v, want %+v", *pay, vectorPay)
	}
	if _, err := parseToken(vectorToken, vectorKey, time.Unix(vectorPay.Exp, 0)); err == nil {
		t.Fatal("a token verified at its expiry instant")
	}
}

func TestTokenRejects(t *testing.T) {
	valid, err := signToken(vectorKey, vectorPay)
	if err != nil {
		t.Fatal(err)
	}
	body, sig, _ := strings.Cut(valid, ".")
	other := vectorPay
	other.Size = 12346
	body2, err := signToken(vectorKey, other)
	if err != nil {
		t.Fatal(err)
	}
	tampered := body2 + "." + sig
	badVersion := vectorPay
	badVersion.V = 2
	versionToken, err := signToken(vectorKey, badVersion)
	if err != nil {
		t.Fatal(err)
	}
	expired, err := signToken(vectorKey, tokenPayload{V: 1, Obj: vectorPay.Obj, Exp: vectorNow.Unix(), Size: 1})
	if err != nil {
		t.Fatal(err)
	}
	badObj, err := signToken(vectorKey, tokenPayload{V: 1, Obj: "not-hex", Exp: vectorPay.Exp, Size: 1})
	if err != nil {
		t.Fatal(err)
	}
	badSize, err := signToken(vectorKey, tokenPayload{V: 1, Obj: vectorPay.Obj, Exp: vectorPay.Exp, Size: -1})
	if err != nil {
		t.Fatal(err)
	}
	shortSig := body + "." + base64.RawURLEncoding.EncodeToString([]byte("short"))
	sigRaw, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		t.Fatal(err)
	}
	sigRaw[0] ^= 0x80
	changedSig := body + "." + base64.RawURLEncoding.EncodeToString(sigRaw)
	cases := map[string]string{
		"valid":           valid,
		"changed payload": tampered,
		"changed sig":     changedSig,
		"bad version":     versionToken,
		"expired":         expired,
		"bad obj":         badObj,
		"negative size":   badSize,
		"short sig":       shortSig,
		"no dot":          body,
		"empty":           "",
		"trailing":        valid + "x",
	}
	for name, token := range cases {
		if _, err := parseToken(token, vectorKey, vectorNow); err == nil && name != "valid" {
			t.Errorf("%s: token accepted", name)
		}
	}
	if _, err := parseToken(valid, vectorKey, vectorNow); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if _, err := parseToken(valid, mustHex(strings.Repeat("11", 32)), vectorNow); err == nil {
		t.Fatal("token verified under the wrong key")
	}
}
