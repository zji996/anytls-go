package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// tokenVersion is the only payload version this build understands; the
// contract in file-channel.md bumps it before changing the format.
const tokenVersion = 1

// tokenPayload is the JSON object carried by /b/<token>. loc is opaque to the
// egress: it is only handed back to the origin inside the token.
type tokenPayload struct {
	V    int    `json:"v"`
	Obj  string `json:"obj"`
	Loc  string `json:"loc"`
	Exp  int64  `json:"exp"`
	Size int64  `json:"size"`
	Type string `json:"type"`
	Disp string `json:"disp"`
}

var errBadToken = errors.New("bad token")

// b64 is base64url without padding, as the contract writes it.
func b64(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }

// signToken signs payload with key: b64(payload) "." b64(HMAC-SHA256(key, b64(payload))).
// Only the tests sign tokens; serve verifies them.
func signToken(key []byte, p tokenPayload) (string, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	encoded := b64(body)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(encoded))
	return encoded + "." + b64(mac.Sum(nil)), nil
}

// parseToken verifies raw and returns its payload. Every failure, whatever the
// cause, is errBadToken: the caller answers 404 without saying why.
func parseToken(raw string, key []byte, now time.Time) (*tokenPayload, error) {
	body, sig, ok := strings.Cut(raw, ".")
	if !ok || body == "" || sig == "" {
		return nil, errBadToken
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || len(got) != sha256.Size {
		return nil, errBadToken
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(body))
	if !hmac.Equal(got, mac.Sum(nil)) {
		return nil, errBadToken
	}
	plain, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, errBadToken
	}
	var p tokenPayload
	if err := json.Unmarshal(plain, &p); err != nil {
		return nil, errBadToken
	}
	if p.V != tokenVersion || p.Exp <= now.Unix() || p.Size < 0 {
		return nil, errBadToken
	}
	if len(p.Obj) != 32 {
		return nil, errBadToken
	}
	if _, err := hex.DecodeString(p.Obj); err != nil {
		return nil, errBadToken
	}
	p.Obj = strings.ToLower(p.Obj)
	return &p, nil
}
