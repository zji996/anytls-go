package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// statusReport is the one-line JSON `blobcache status` prints.
type statusReport struct {
	Locked      bool   `json:"locked"`
	Objects     int    `json:"objects"`
	UsedBytes   int64  `json:"used_bytes"`
	SizeBytes   int64  `json:"size_bytes"`
	Hits        int64  `json:"hits"`
	Misses      int64  `json:"misses"`
	Passthrough int64  `json:"passthrough"`
	OriginBytes int64  `json:"origin_bytes"`
	ServedBytes int64  `json:"served_bytes"`
	StartedAt   string `json:"started_at"`
}

func (s *server) status() statusReport {
	objects, used := s.cache.stats()
	return statusReport{
		Locked:      s.cache.locked(),
		Objects:     objects,
		UsedBytes:   used,
		SizeBytes:   s.cache.capacity,
		Hits:        s.counters.hits.Load(),
		Misses:      s.counters.misses.Load(),
		Passthrough: s.counters.passthrough.Load(),
		OriginBytes: s.counters.originBytes.Load(),
		ServedBytes: s.counters.servedBytes.Load(),
		StartedAt:   s.started.UTC().Format(time.RFC3339),
	}
}

// controlMux is the small protocol the unix socket speaks: unlock with the
// master key, and report status. ssh egress blobcache unlock -socket ... < key
// is the only writer, so a plain HTTP request per command is enough.
func (s *server) controlMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /unlock", s.handleUnlock)
	mux.HandleFunc("GET /status", s.handleStatus)
	return mux
}

func (s *server) handleUnlock(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		http.Error(w, "cannot read key", http.StatusBadRequest)
		return
	}
	key, err := parseHexKey(string(body))
	if err != nil {
		log.Printf("blobcache: unlock refused: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.cache.setKey(key); err != nil {
		log.Printf("blobcache: unlock refused: %v", err)
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	log.Printf("blobcache: unlocked; caching %d bytes in %s", s.cache.capacity, s.cache.dir)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	body, err := json.Marshal(s.status())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(append(body, '\n'))
}

// listenControl binds the control unix socket with mode 0600.
func listenControl(path string) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil, fmt.Errorf("%s is in use by another process", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		os.Remove(path)
		return nil, err
	}
	return ln, nil
}

// controlRequest talks to the control socket of a running blobcache.
func controlRequest(socket, method, path string, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://blobcache"+path, reader)
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}},
	}
	defer client.CloseIdleConnections()
	return client.Do(req)
}

// unlockKey reads the master key from in and sends it to the running process.
// The key travels over the unix socket, never over the network or a file.
func unlockKey(socket string, in io.Reader) error {
	raw, err := io.ReadAll(io.LimitReader(in, 4096))
	if err != nil {
		return fmt.Errorf("read key: %w", err)
	}
	text := strings.TrimSpace(string(raw))
	if _, err := parseHexKey(text); err != nil {
		return fmt.Errorf("master key: %w", err)
	}
	resp, err := controlRequest(socket, http.MethodPost, "/unlock", []byte(text))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// fetchStatus returns the one-line JSON status report of the running process.
func fetchStatus(socket string) (statusReport, error) {
	var report statusReport
	resp, err := controlRequest(socket, http.MethodGet, "/status", nil)
	if err != nil {
		return report, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return report, err
	}
	if resp.StatusCode != http.StatusOK {
		return report, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, &report); err != nil {
		return report, err
	}
	return report, nil
}

// parseHexKey accepts one 64-hexadecimal-character key, surrounding whitespace
// ignored, as the three key files and unlock's stdin do.
func parseHexKey(s string) ([]byte, error) {
	text := strings.TrimSpace(s)
	if len(text) != 64 {
		return nil, fmt.Errorf("want 64 hexadecimal characters, got %d", len(text))
	}
	key, err := hex.DecodeString(text)
	if err != nil {
		return nil, errors.New("not hexadecimal")
	}
	return key, nil
}

func readHexKeyFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := parseHexKey(string(raw))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return key, nil
}
