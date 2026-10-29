package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

func TestUnlockCommandTurnsOnCaching(t *testing.T) {
	h := newHarness(t, options{locked: true})
	socket := h.controlSocket()
	body := makeBody(9000, 53)
	token := h.put(testObj, body, "text/plain", "inline")

	if st, err := fetchStatus(socket); err != nil {
		t.Fatalf("status: %v", err)
	} else if !st.Locked || st.SizeBytes != h.opts.size {
		t.Fatalf("locked status %+v", st)
	}
	if got := h.get(token, ""); got.status != 200 {
		t.Fatalf("locked request: %d", got.status)
	}
	// The form the timer uses: ssh egress blobcache unlock -socket ... < key
	key := strings.ToUpper(hex.EncodeToString(testMaster)) + "\n"
	if err := unlockKey(socket, strings.NewReader(key)); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	st, err := fetchStatus(socket)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Locked {
		t.Fatal("still locked after unlock")
	}
	// Unlocking again with the same key is fine; the cache keeps working.
	if err := unlockKey(socket, strings.NewReader(hex.EncodeToString(testMaster))); err != nil {
		t.Fatalf("second unlock: %v", err)
	}
	if got := h.get(token, ""); got.status != 200 || !bytes.Equal(got.body, body) {
		t.Fatalf("unlocked request: %d", got.status)
	}
	h.waitObjects(1)
}

func TestUnlockRefusesAnotherKey(t *testing.T) {
	h := newHarness(t, options{locked: true})
	socket := h.controlSocket()
	body := makeBody(4000, 59)
	token := h.put(testObj, body, "text/plain", "inline")
	if got := h.get(token, ""); got.status != 200 {
		t.Fatalf("locked request: %d", got.status)
	}
	if err := unlockKey(socket, strings.NewReader(hex.EncodeToString(testMaster))); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if got := h.get(token, ""); got.status != 200 {
		t.Fatalf("fill: %d", got.status)
	}
	h.waitObjects(1)
	other := strings.Repeat("77", 32)
	if err := unlockKey(socket, strings.NewReader(other)); err == nil {
		t.Fatal("a second, different key was accepted")
	}
	// The first key is still in force: the cached object still serves.
	if got := h.get(token, ""); got.status != 200 || !bytes.Equal(got.body, body) {
		t.Fatalf("after refused unlock: %d", got.status)
	}
	if n := h.orig.count(); n != 2 {
		t.Fatalf("origin saw %d requests, want 2: the key was replaced", n)
	}
}

func TestUnlockRejectsMalformedKey(t *testing.T) {
	h := newHarness(t, options{locked: true})
	socket := h.controlSocket()
	for _, bad := range []string{"", "0f", strings.Repeat("0f", 31), strings.Repeat("0f", 33), strings.Repeat("zz", 32)} {
		if err := unlockKey(socket, strings.NewReader(bad)); err == nil {
			t.Errorf("key %q accepted", bad)
		}
	}
	if st, err := fetchStatus(socket); err != nil {
		t.Fatalf("status: %v", err)
	} else if !st.Locked {
		t.Fatal("a malformed key unlocked the cache")
	}
}

func TestStatusIsOneJSONLine(t *testing.T) {
	h := newHarness(t, options{})
	socket := h.controlSocket()
	token := h.put(testObj, makeBody(3000, 61), "text/plain", "inline")
	if got := h.get(token, ""); got.status != 200 {
		t.Fatalf("request: %d", got.status)
	}
	h.waitObjects(1)
	resp, err := controlRequest(socket, "GET", "/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSuffix(string(raw), "\n")
	if strings.Contains(line, "\n") {
		t.Fatalf("status is not one line: %q", line)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(line), &fields); err != nil {
		t.Fatalf("status is not JSON: %v", err)
	}
	for _, key := range []string{"locked", "objects", "used_bytes", "size_bytes", "hits", "misses", "passthrough", "origin_bytes", "served_bytes", "started_at"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("status lacks %q: %s", key, line)
		}
	}
	if len(fields) != 10 {
		t.Errorf("status has %d keys: %s", len(fields), line)
	}
	// The counters settle just after the client has its last byte.
	h.waitStatus(func(st statusReport) bool {
		return st.Objects == 1 && st.Misses == 1 && st.OriginBytes == 3000 && st.ServedBytes == 3000 && !st.Locked
	})
	if _, err := os.Stat(h.socket); err != nil {
		t.Fatalf("control socket: %v", err)
	}
	if info, err := os.Stat(h.socket); err == nil && info.Mode().Perm() != 0o600 {
		t.Fatalf("control socket mode %v", info.Mode().Perm())
	}
}

func TestCommandsRequireSocket(t *testing.T) {
	if err := runUnlock(nil); err == nil {
		t.Error("unlock without -socket succeeded")
	}
	if err := runStatus(nil); err == nil {
		t.Error("status without -socket succeeded")
	}
	if err := runUnlock([]string{"-socket", "/nonexistent/blobcache.sock"}); err == nil {
		t.Error("unlock against a missing socket succeeded")
	}
	if err := runStatus([]string{"-socket", "/nonexistent/blobcache.sock"}); err == nil {
		t.Error("status against a missing socket succeeded")
	}
}
