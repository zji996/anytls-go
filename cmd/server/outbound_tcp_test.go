package main

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestDedupStreamReachesUnixBackend(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "dedup.sock")
	backend, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		c, err := backend.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()

	client, stream := net.Pipe()
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go proxyOutboundDedup(ctx, stream, "unix:"+socket)

	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(client, "ping"); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "ping" {
		t.Fatalf("echo through backend = %q", got)
	}
}

func TestDedupStreamWithoutBackendIsRefused(t *testing.T) {
	client, stream := net.Pipe()
	defer client.Close()
	defer stream.Close()
	if err := proxyOutboundDedup(context.Background(), stream, ""); err == nil {
		t.Fatal("stream without a dedup backend was accepted")
	}
}

func TestParseServerConfigReadsDedupBackend(t *testing.T) {
	t.Setenv("ANYTLS_DEDUP_BACKEND", "unix:/run/anytls-dedup.sock")
	config, err := parseServerConfig([]string{"-p", "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if config.dedupBackend != "unix:/run/anytls-dedup.sock" {
		t.Fatalf("dedup backend = %q", config.dedupBackend)
	}
}
