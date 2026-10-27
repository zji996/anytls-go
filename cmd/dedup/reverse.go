package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// The reverse modes put the same pair in front of plain HTTP services that a
// TCP tunnel such as frp publishes, with no TLS to intercept:
//
//	client -> Caddy -> encode (egress) -> frps ~ frpc -> decode (LAN) -> service
//
// One encoder per egress forwards every deduplicated service through one
// tunnel port; the decoder picks the service by Host from a route file that
// SIGHUP reloads, so adding a service restarts neither end.

func runEncode(args []string) {
	fs := flag.NewFlagSet("encode", flag.ExitOnError)
	listen := fs.String("listen", "", "HTTP listen address for the reverse proxy in front of the tunnel")
	upstream := fs.String("upstream", "", "the decoder, e.g. http://127.0.0.1:10900 (a tunnel port)")
	admin := fs.String("admin", "", "loopback address for GET /stats")
	indexMiB := fs.Int64("index-mib", 900, "content indexed as held by the decoder; keep at about 90% of its -cache-mib")
	maxBodyMiB := fs.Int64("max-body-mib", 64, "largest body that is deduplicated; larger ones pass through")
	inflightMiB := fs.Int64("inflight-mib", 256, "memory for requests being deduplicated, 2x body + 2 MiB each; others pass through")
	inflightWait := fs.Duration("inflight-wait", 3*time.Second, "how long a request may wait for -inflight-mib before it passes through")
	memoryMiB := fs.Int64("memory-mib", 0, "Go soft memory limit (0 = index/8 + inflight + 64; GOMEMLIMIT overrides)")
	oomScore := fs.Int("oom-score-adj", 500, "raise oom_score_adj to this so the kernel kills the encoder before the proxies (0 = leave)")
	compressFlag := fs.String("compress", "auto", "CPU for content the decoder lacks: auto, huffman (cheaper) or off (cache only)")
	probe := fs.Duration("probe-interval", 5*time.Second, "decoder health check interval")
	drain := fs.Duration("drain", 30*time.Second, "on SIGTERM, time for open requests to finish")
	fs.Parse(args)
	if *listen == "" || *upstream == "" {
		log.Fatal("-listen and -upstream are required")
	}
	up, err := url.Parse(*upstream)
	if err != nil || up.Scheme != "http" || up.Host == "" {
		log.Fatalf("bad -upstream %q: want http://host:port", *upstream)
	}
	mode, err := parseCompression(*compressFlag)
	if err != nil {
		log.Fatal(err)
	}
	if *memoryMiB == 0 {
		*memoryMiB = *indexMiB/8 + *inflightMiB + 64
	}
	setMemoryLimit(*memoryMiB)
	preferOOMKill(*oomScore)

	enc := newEncoder(route{url: up}, *indexMiB<<20, *maxBodyMiB<<20)
	enc.inflight = &budget{limit: *inflightMiB << 20}
	enc.inflightWait = *inflightWait
	enc.compress = mode
	go probeLoop(enc, up, *probe, nil)
	serveAdmin(*admin, enc.snapshot)

	ln, err := bind(*listen)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Handler: enc, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}
	log.Printf("dedup encode on %s -> %s", *listen, up)
	go func() {
		if err := server.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	untilSignal(*drain, func(ctx context.Context) { server.Shutdown(ctx) })
}

func runDecode(args []string) {
	fs := flag.NewFlagSet("decode", flag.ExitOnError)
	listen := fs.String("listen", "", "listen address the tunnel delivers to")
	routes := fs.String("routes", "", "file of \"host http://service\" lines; SIGHUP reloads it")
	admin := fs.String("admin", "", "loopback address for GET /stats")
	cacheMiB := fs.Int64("cache-mib", 1024, "chunk bytes kept; they cost about 1.25x this")
	maxBodyMiB := fs.Int64("max-body-mib", 64, "largest body that is deduplicated")
	inflightMiB := fs.Int64("inflight-mib", 256, "memory for frames being rebuilt; past it the encoder resends originals, which stream")
	memoryMiB := fs.Int64("memory-mib", 0, "Go soft memory limit (0 = cache*1.25 + inflight + 64; GOMEMLIMIT overrides)")
	oomScore := fs.Int("oom-score-adj", 500, "raise oom_score_adj to this so the kernel kills the decoder first (0 = leave)")
	drain := fs.Duration("drain", 30*time.Second, "on SIGTERM, time for open requests to finish")
	fs.Parse(args)
	if *listen == "" || *routes == "" {
		log.Fatal("-listen and -routes are required")
	}
	table := &routeTable{}
	if err := table.load(*routes); err != nil {
		log.Fatal(err)
	}
	if *memoryMiB == 0 {
		*memoryMiB = *cacheMiB*5/4 + *inflightMiB + 64
	}
	setMemoryLimit(*memoryMiB)
	preferOOMKill(*oomScore)

	dec := newDecoder(route{table: table}, *cacheMiB<<20, *maxBodyMiB<<20)
	dec.inflight.limit = *inflightMiB << 20
	serveAdmin(*admin, dec.snapshot)
	go reloadOnHangup(table, *routes)

	ln, err := bind(*listen)
	if err != nil {
		log.Fatal(err)
	}
	server := newDecoderServer(dec)
	log.Printf("dedup decode on %s for %s", *listen, strings.Join(table.hosts(), ","))
	go func() {
		if err := server.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	untilSignal(*drain, func(ctx context.Context) { server.Shutdown(ctx) })
}

// reloadOnHangup rereads the route file on SIGHUP. A file that does not parse
// leaves the routes in use as they were.
func reloadOnHangup(table *routeTable, path string) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	for range hup {
		if err := table.load(path); err != nil {
			log.Printf("routes not reloaded: %v", err)
			continue
		}
		log.Printf("routes reloaded: %s", strings.Join(table.hosts(), ","))
	}
}

// routeTable maps a request's Host to the plain HTTP service behind it.
type routeTable struct {
	m atomic.Pointer[map[string]*url.URL]
}

func (t *routeTable) load(path string) error {
	text, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	m, err := parseRoutes(string(text))
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	t.m.Store(&m)
	return nil
}

// lookup returns the service for host, or nil.
func (t *routeTable) lookup(host string) *url.URL {
	m := t.m.Load()
	if m == nil {
		return nil
	}
	return (*m)[strings.TrimSuffix(strings.ToLower(hostname(host)), ".")]
}

func (t *routeTable) hosts() []string {
	var out []string
	if m := t.m.Load(); m != nil {
		for h := range *m {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}

// parseRoutes reads "host http://service" lines; blank lines and # comments
// are skipped. An empty table is valid: every host is then refused.
func parseRoutes(text string) (map[string]*url.URL, error) {
	m := map[string]*url.URL{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for n := 1; sc.Scan(); n++ {
		line, _, _ := strings.Cut(sc.Text(), "#")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("line %d: want \"host http://service\"", n)
		}
		host, err := parseDomains(fields[0])
		if err != nil || len(host) != 1 {
			return nil, fmt.Errorf("line %d: bad host %q", n, fields[0])
		}
		u, err := url.Parse(fields[1])
		if err != nil || u.Scheme != "http" || u.Host == "" || (u.Path != "" && u.Path != "/") {
			return nil, fmt.Errorf("line %d: bad service %q: want http://host:port", n, fields[1])
		}
		if _, dup := m[host[0]]; dup {
			return nil, fmt.Errorf("line %d: %s routed twice", n, host[0])
		}
		m[host[0]] = u
	}
	return m, sc.Err()
}
