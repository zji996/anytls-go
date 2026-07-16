package main

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	M "github.com/sagernet/sing/common/metadata"
)

func TestWithDefaultPort(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "domain without port",
			in:   "example.com",
			want: "example.com:443",
		},
		{
			name: "domain with port",
			in:   "example.com:8443",
			want: "example.com:8443",
		},
		{
			name: "ipv6 without port",
			in:   "2001:db8::1",
			want: "[2001:db8::1]:443",
		},
		{
			name: "bracketed ipv6 without port",
			in:   "[2001:db8::1]",
			want: "[2001:db8::1]:443",
		},
		{
			name: "ipv6 with port",
			in:   "[2001:db8::1]:8443",
			want: "[2001:db8::1]:8443",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := withDefaultPort(tt.in, "443")
			if got != tt.want {
				t.Fatalf("withDefaultPort(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

type directProbeClient struct {
	err error
}

func (c directProbeClient) CreateProxy(ctx context.Context, destination M.Socksaddr) (net.Conn, error) {
	if c.err != nil {
		return nil, c.err
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", destination.String())
}

func TestRunHealthProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runHealthProbe(ctx, directProbeClient{}); err != nil {
		t.Fatal(err)
	}
}

func TestRunHealthProbeReportsProxyFailure(t *testing.T) {
	want := errors.New("dial failed")
	err := runHealthProbe(context.Background(), directProbeClient{err: want})
	if !errors.Is(err, want) {
		t.Fatalf("runHealthProbe error = %v, want %v", err, want)
	}
}

type corruptProbeClient struct{}

func (corruptProbeClient) CreateProxy(context.Context, M.Socksaddr) (net.Conn, error) {
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		request := make([]byte, 32)
		if _, err := io.ReadFull(server, request); err == nil {
			_, _ = server.Write(make([]byte, len(request)))
		}
	}()
	return client, nil
}

func TestRunHealthProbeRejectsMismatchedPayload(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runHealthProbe(ctx, corruptProbeClient{}); err == nil {
		t.Fatal("runHealthProbe accepted a mismatched payload")
	}
}

type blockedProbeClient struct{}

func (blockedProbeClient) CreateProxy(ctx context.Context, _ M.Socksaddr) (net.Conn, error) {
	client, server := net.Pipe()
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	return client, nil
}

func TestRunHealthProbeHonorsTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := runHealthProbe(ctx, blockedProbeClient{}); err == nil {
		t.Fatal("runHealthProbe ignored its timeout")
	}
}
