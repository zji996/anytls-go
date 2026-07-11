package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAuthenticateConnectionReadsFragmentedRequest(t *testing.T) {
	originalPassword := slices.Clone(passwordSha256)
	defer func() { passwordSha256 = originalPassword }()
	sum := sha256.Sum256([]byte("secret"))
	passwordSha256 = sum[:]

	request := make([]byte, 34+9)
	copy(request, passwordSha256)
	binary.BigEndian.PutUint16(request[32:34], 9)
	copy(request[34:], "123456789")

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	writeErr := make(chan error, 1)
	go func() {
		for _, chunk := range [][]byte{request[:7], request[7:33], request[33:37], request[37:]} {
			if _, err := clientConn.Write(chunk); err != nil {
				writeErr <- err
				return
			}
		}
		writeErr <- nil
	}()

	conn, authenticated, canFallback, err := authenticateConnection(serverConn)
	if err != nil {
		t.Fatal(err)
	}
	if conn != serverConn || !authenticated || canFallback {
		t.Fatalf("authenticate result: authenticated=%v fallback=%v", authenticated, canFallback)
	}
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticateConnectionReplaysRejectedData(t *testing.T) {
	originalPassword := slices.Clone(passwordSha256)
	defer func() { passwordSha256 = originalPassword }()
	sum := sha256.Sum256([]byte("secret"))
	passwordSha256 = sum[:]

	payload := []byte("GET /probe HTTP/1.1\r\nHost: example.com\r\n\r\n")
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		_, _ = clientConn.Write(payload)
		_ = clientConn.Close()
	}()

	conn, authenticated, canFallback, err := authenticateConnection(serverConn)
	if err != nil {
		t.Fatal(err)
	}
	if authenticated || !canFallback {
		t.Fatalf("authenticate result: authenticated=%v fallback=%v", authenticated, canFallback)
	}
	replayed, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if string(replayed) != string(payload) {
		t.Fatalf("replayed data = %q, want %q", replayed, payload)
	}
}

func TestPlainTCPProbeFallsBack(t *testing.T) {
	fallbackListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer fallbackListener.Close()

	fallbackDone := make(chan string, 1)
	go func() {
		conn, err := fallbackListener.Accept()
		if err != nil {
			fallbackDone <- err.Error()
			return
		}
		defer conn.Close()

		req, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			fallbackDone <- err.Error()
			return
		}
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nOK")
		fallbackDone <- req
	}()

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	server := NewMyServer(&tls.Config{}, fallbackListener.Addr().String())
	go handleTcpConnection(ctx, serverConn, server)

	_ = clientConn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(clientConn, "GET /probe HTTP/1.1\r\nHost: example.com\r\n\r\n"); err != nil {
		t.Fatal(err)
	}

	response := make([]byte, 64)
	n, err := clientConn.Read(response)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(response[:n]), "200 OK") {
		t.Fatalf("unexpected fallback response: %q", string(response[:n]))
	}

	select {
	case req := <-fallbackDone:
		if !strings.HasPrefix(req, "GET /probe ") {
			t.Fatalf("fallback did not receive replayed request, got %q", req)
		}
	case <-ctx.Done():
		t.Fatal("fallback listener did not receive request")
	}
}
