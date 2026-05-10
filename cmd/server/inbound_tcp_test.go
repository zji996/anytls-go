package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

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
