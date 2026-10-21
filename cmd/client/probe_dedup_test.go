package main

import (
	"anytls/proxy/padding"
	"anytls/proxy/session"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	M "github.com/sagernet/sing/common/metadata"
)

// Run main in a subprocess so exit status and the installer-facing stdout
// contract are exercised together with the real TLS/AnyTLS authentication.
func TestDedupProbeProcess(t *testing.T) {
	if os.Getenv("ANYTLS_TEST_PROBE_MAIN") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append(os.Args[:1], os.Args[i+1:]...)
			break
		}
	}
	flag.CommandLine = flag.NewFlagSet("anytls-client", flag.ExitOnError)
	main()
	os.Exit(0)
}

func probeCommand(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestDedupProbeProcess$", "--"}, args...)...)
	cmd.Env = append(os.Environ(), "ANYTLS_TEST_PROBE_MAIN=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func TestDedupProbeCLI(t *testing.T) {
	const password = "probe-secret-do-not-print"
	passwordFile := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(passwordFile, []byte(password+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, response, want string
	}{
		{"epoch", "204 No Content\r\nX-Edge-Dedup-Epoch: 0123456789abcdef\r\n\r\n", "0123456789abcdef\n"},
		{"wrong status", "200 OK\r\nX-Edge-Dedup-Epoch: 0123456789abcdef\r\nContent-Length: 0\r\n\r\n", ""},
		{"missing epoch", "204 No Content\r\n\r\n", ""},
		{"empty epoch", "204 No Content\r\nX-Edge-Dedup-Epoch:\r\n\r\n", ""},
		{"invalid epoch", "204 No Content\r\nX-Edge-Dedup-Epoch: two epochs\r\n\r\n", ""},
		{"duplicate epoch", "204 No Content\r\nX-Edge-Dedup-Epoch: one\r\nX-Edge-Dedup-Epoch: two\r\n\r\n", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			addr, done := dedupProbeServer(t, password, "HTTP/1.1 "+tt.response)
			stdout, stderr, err := probeCommand(t, "-s", addr, "-password-file", passwordFile, "-insecure=true", "-probe-dedup")
			if (err == nil) != (tt.want != "") || stdout != tt.want {
				t.Fatalf("exit %v, stdout %q, stderr %q; want stdout %q", err, stdout, stderr, tt.want)
			}
			if strings.Contains(stdout+stderr, password) {
				t.Fatal("probe output exposed the password")
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("probe did not close its AnyTLS session")
			}
		})
	}
}

func dedupProbeServer(t *testing.T, password, response string) (string, <-chan error) {
	t.Helper()
	certServer := httptest.NewTLSServer(http.NotFoundHandler())
	config := certServer.TLS.Clone()
	certServer.Close()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		auth := make([]byte, 34)
		if _, err := io.ReadFull(conn, auth); err != nil {
			done <- err
			return
		}
		sum := sha256.Sum256([]byte(password))
		if !bytes.Equal(auth[:32], sum[:]) {
			done <- errors.New("incorrect AnyTLS authentication")
			return
		}
		if _, err := io.CopyN(io.Discard, conn, int64(binary.BigEndian.Uint16(auth[32:]))); err != nil {
			done <- err
			return
		}
		requestResult := make(chan error, 1)
		sess := session.NewServerSession(conn, func(stream *session.Stream) {
			defer stream.Close()
			dest, err := M.SocksaddrSerializer.ReadAddrPort(stream)
			if err == nil && dest.String() != dedupProbeHost+":80" {
				err = fmt.Errorf("unexpected destination %s", dest)
			}
			if err == nil {
				var req *http.Request
				req, err = http.ReadRequest(bufio.NewReader(stream))
				if err == nil && (req.Method != "GET" || req.RequestURI != "/health" || req.Host != dedupProbeHost) {
					err = fmt.Errorf("unexpected request: %s %s Host %s", req.Method, req.RequestURI, req.Host)
				}
			}
			if err == nil {
				_, err = io.WriteString(stream, response)
			}
			requestResult <- err
		}, padding.NewDefaultPaddingFactory())
		sess.Run()
		sess.Close()
		select {
		case err := <-requestResult:
			done <- err
		case <-time.After(time.Second):
			done <- errors.New("session ended without a health request")
		}
	}()
	return ln.Addr().String(), done
}

func TestDedupProbeFlags(t *testing.T) {
	stdout, stderr, err := probeCommand(t, "-probe", "-probe-dedup")
	if err == nil || stdout != "" || !strings.Contains(stderr, "set only one of -probe and -probe-dedup") {
		t.Fatalf("mutually exclusive flags: exit %v, stdout %q, stderr %q", err, stdout, stderr)
	}
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	stdout, _, err = probeCommand(t, "-s", server.Listener.Addr().String(), "-p", "secret", "-insecure=false", "-probe-dedup")
	if err == nil || stdout != "" {
		t.Fatalf("probe ignored TLS verification: exit %v, stdout %q", err, stdout)
	}
}

type stalledDedupClient struct {
	stage  string
	closed atomic.Bool
	stream *trackedProbeConn
}

type trackedProbeConn struct {
	net.Conn
	closed atomic.Bool
}

func (c *trackedProbeConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

func (c *stalledDedupClient) Close() error {
	c.closed.Store(true)
	return nil
}

func (c *stalledDedupClient) CreateProxy(ctx context.Context, _ M.Socksaddr) (net.Conn, error) {
	if c.stage == "create" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	client, server := net.Pipe()
	c.stream = &trackedProbeConn{Conn: client}
	go func() {
		defer server.Close()
		if c.stage == "read" {
			_, _ = http.ReadRequest(bufio.NewReader(server))
		}
		<-ctx.Done()
	}()
	return c.stream, nil
}

func TestDedupProbeTimeoutClosesResources(t *testing.T) {
	for _, stage := range []string{"create", "write", "read"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			client := &stalledDedupClient{stage: stage}
			var stdout bytes.Buffer
			start := time.Now()
			err := runOneShotProbe(ctx, client, true, &stdout)
			if err == nil || stdout.Len() != 0 || time.Since(start) > time.Second {
				t.Fatalf("timeout: error %v, stdout %q, elapsed %s", err, stdout.String(), time.Since(start))
			}
			if !client.closed.Load() || (client.stream != nil && !client.stream.closed.Load()) {
				t.Fatal("probe left client or stream open")
			}
		})
	}
}
