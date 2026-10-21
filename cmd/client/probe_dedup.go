package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	M "github.com/sagernet/sing/common/metadata"
)

const dedupProbeHost = "dedup.anytls.arpa"

type probeClient interface {
	proxyCreator
	Close() error
}

// Return before main reports failures: logrus.Fatal exits without running defers.
func runOneShotProbe(ctx context.Context, client probeClient, dedup bool, stdout io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	defer client.Close()
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stop()
	if !dedup {
		return runHealthProbe(ctx, client)
	}
	epoch, err := runDedupProbe(ctx, client)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, epoch)
	return err
}

func runDedupProbe(ctx context.Context, client proxyCreator) (string, error) {
	conn, err := client.CreateProxy(ctx, M.ParseSocksaddr(dedupProbeHost+":80"))
	if err != nil {
		return "", fmt.Errorf("create AnyTLS dedup probe stream: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return "", fmt.Errorf("set dedup probe deadline: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+dedupProbeHost+"/health", nil)
	if err != nil {
		return "", err
	}
	req.Close = true
	if err := req.Write(conn); err != nil {
		return "", fmt.Errorf("write dedup health request: %w", err)
	}
	// Only headers are needed; bound their size as well as the total probe time.
	resp, err := http.ReadResponse(bufio.NewReader(io.LimitReader(conn, 64<<10)), req)
	if err != nil {
		return "", fmt.Errorf("read dedup health response: %w", err)
	}
	// Closing the stream releases any response body without draining an
	// untrusted error response (whose body may never finish).
	if resp.StatusCode != http.StatusNoContent {
		return "", fmt.Errorf("dedup health status %d, want 204", resp.StatusCode)
	}
	epochs := resp.Header.Values("X-Edge-Dedup-Epoch")
	if len(epochs) != 1 || !validDedupEpoch(epochs[0]) {
		return "", fmt.Errorf("missing or invalid dedup epoch")
	}
	return epochs[0], nil
}

// Treat epochs as opaque HTTP tokens, safe to emit as a single stdout line.
func validDedupEpoch(epoch string) bool {
	if epoch == "" {
		return false
	}
	for _, c := range epoch {
		if c < 0x21 || c > 0x7e || strings.ContainsRune("()<>@,;:\\\"/[]?={}", c) {
			return false
		}
	}
	return true
}
