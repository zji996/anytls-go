// Command dedup stops repeated request content, such as an agent resending its
// whole conversation every turn, from crossing an anytls tunnel twice.
//
//	client -> dedup front (router) ~ anytls ~ server -> dedup back (exit) -> HTTPS origin
//
// The front terminates TLS to allowlisted names with a private CA the client
// trusts, chunks each request body and sends a 32-byte reference for every
// chunk the back already holds. The back keeps chunk bytes in a byte-bounded
// LRU, rebuilds the request and sends it to the origin over HTTPS. A miss
// answers 409 and the front resends the original, so a cold or restarted back
// only costs bandwidth. Responses stream through untouched.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/protocol/socks"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: dedup front|back|ca [flags]")
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "front":
		runFront(os.Args[2:])
	case "back":
		runBack(os.Args[2:])
	case "ca":
		runCA(os.Args[2:])
	default:
		usage()
	}
}

func mustDomains(s string) domainList {
	d, err := parseDomains(s)
	if err != nil {
		log.Fatalf("-domains: %v", err)
	}
	return d
}

func runCA(args []string) {
	fs := flag.NewFlagSet("ca", flag.ExitOnError)
	dir := fs.String("dir", "", "directory for ca.pem and ca.key")
	domains := fs.String("domains", "", "comma-separated domains the CA may certify")
	fs.Parse(args)
	if *dir == "" {
		log.Fatal("-dir is required")
	}
	if err := createCA(*dir, mustDomains(*domains)); err != nil {
		log.Fatal(err)
	}
	fmt.Println(*dir + "/" + caCertFile)
}

func runFront(args []string) {
	fs := flag.NewFlagSet("front", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:10860", "SOCKS5/HTTP proxy listen address; a SOCKS5 username and password pick the exit, see -via")
	via := fs.String("via", "", "SOCKS5 proxy toward the exit, e.g. socks5://127.0.0.1:10808; dialed with the client's SOCKS5 username and password when it sent them")
	decoder := fs.String("decoder", "http://"+dedupHost, "decoder URL, reached through -via")
	domains := fs.String("domains", "", "comma-separated domains whose TLS is intercepted")
	caDir := fs.String("ca-dir", "", "directory with ca.pem and ca.key")
	admin := fs.String("admin", "", "loopback address for GET /stats")
	indexMiB := fs.Int64("index-mib", 230, "content indexed as held by each exit's decoder; keep at about 90% of its -cache-mib; costs up to an eighth of it")
	maxBodyMiB := fs.Int64("max-body-mib", 64, "largest body that is deduplicated; larger ones pass through")
	inflightMiB := fs.Int64("inflight-mib", 128, "memory for requests being deduplicated, 2x body + 2 MiB each; others pass through")
	inflightWait := fs.Duration("inflight-wait", 3*time.Second, "how long a request may wait for -inflight-mib before it passes through")
	maxConns := fs.Int("max-conns", 256, "open proxy connections; more are refused (0 = no limit)")
	memoryMiB := fs.Int64("memory-mib", 0, "Go soft memory limit (0 = index/8 + inflight + 64; GOMEMLIMIT overrides)")
	oomScore := fs.Int("oom-score-adj", 500, "raise oom_score_adj to this so the kernel kills the front before the proxy (0 = leave)")
	compressFlag := fs.String("compress", "auto", "CPU for content the decoder lacks: auto, huffman (cheaper) or off (cache only)")
	probe := fs.Duration("probe-interval", 5*time.Second, "decoder health check interval")
	drain := fs.Duration("drain", 30*time.Second, "on SIGTERM, time for open requests to finish; a successor may already listen")
	linger := fs.Duration("linger", 0, "on SIGTERM, keep accepting this long first, for a successor started at the same time to bind")
	fs.Parse(args)
	if *via == "" || *caDir == "" {
		log.Fatal("-via and -ca-dir are required")
	}
	if *memoryMiB == 0 {
		*memoryMiB = *indexMiB/8 + *inflightMiB + 64
	}
	setMemoryLimit(*memoryMiB)
	preferOOMKill(*oomScore)
	// On an otherwise idle 4-core Cortex-A53 router, GC mark workers on idle
	// Ps doubled the front's CPU with no gain in latency.
	if os.Getenv("GOMAXPROCS") == "" {
		runtime.GOMAXPROCS(1)
	}
	viaURL, err := url.Parse(*via)
	if err != nil || viaURL.Scheme != "socks5" || viaURL.Host == "" {
		log.Fatalf("bad -via %q: want socks5://host:port", *via)
	}
	decURL, err := url.Parse(*decoder)
	if err != nil || decURL.Host == "" {
		log.Fatalf("bad -decoder %q", *decoder)
	}
	list := mustDomains(*domains)
	ca, err := loadAuthority(*caDir)
	if err != nil {
		log.Fatalf("load CA: %v", err)
	}
	if err := ca.check(list); err != nil {
		log.Fatal(err)
	}

	mode, err := parseCompression(*compressFlag)
	if err != nil {
		log.Fatal(err)
	}
	lc := laneConfig{
		via: viaURL, decoder: decURL, indexBytes: *indexMiB << 20, maxBody: *maxBodyMiB << 20,
		inflight: &budget{limit: *inflightMiB << 20}, inflightWait: *inflightWait, compress: mode, probe: *probe,
	}
	f := newFront(list, ca, newLaneSet(maxLanes, lc.newLane))
	f.maxConns = *maxConns
	serveAdmin(*admin, f.snapshot)

	ln, err := bind(*listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("dedup front on %s for %s via %s -> %s", *listen, strings.Join(list, ","), viaURL.Host, decURL)
	go func() {
		if err := f.serve(ln); !errors.Is(err, net.ErrClosed) {
			log.Fatal(err)
		}
	}()
	untilSignal(*drain, func(ctx context.Context) {
		time.Sleep(*linger)
		ln.Close()
		f.shutdown(ctx)
	})
}

// maxLanes bounds the exits one front keeps an encoder and index for.
const maxLanes = 32

type laneConfig struct {
	via, decoder        *url.URL
	indexBytes, maxBody int64
	inflight            *budget
	inflightWait        time.Duration
	compress            compression
	probe               time.Duration
}

// newLane makes a lane that reaches -via with the client's SOCKS5 username and
// password, or with -via's own when the client sent none.
func (c laneConfig) newLane(user, pass string) *lane {
	via := *c.via
	if user != "" || pass != "" {
		via.User = url.UserPassword(user, pass)
	}
	var viaUser, viaPass string
	if via.User != nil {
		viaUser = via.User.Username()
		viaPass, _ = via.User.Password()
	}
	socksClient := socks.NewClient(N.SystemDialer, M.ParseSocksaddr(via.Host), socks.Version5, viaUser, viaPass)
	enc := newEncoder(route{url: c.decoder, via: &via}, c.indexBytes, c.maxBody)
	enc.inflight = c.inflight
	enc.inflightWait = c.inflightWait
	enc.compress = c.compress
	direct := newProxy(route{via: &via})
	enc.origin = direct.Transport
	log.Printf("lane %q via %s", user, via.Host)
	probed := make(chan struct{})
	go probeLoop(enc, c.decoder, c.probe, probed)
	return &lane{name: user, enc: enc, direct: direct, probed: probed, dial: func(ctx context.Context, dest M.Socksaddr) (net.Conn, error) {
		return socksClient.DialContext(ctx, N.NetworkTCP, dest)
	}}
}

// probeLoop checks the decoder every interval while it answers, and backs off
// to a minute while it does not: most exits of a router may have none. It
// closes probed after the first check.
func probeLoop(enc *encoder, decoder *url.URL, interval time.Duration, probed chan<- struct{}) {
	wait := interval
	for {
		enc.probe(context.Background(), decoder)
		if probed != nil {
			close(probed)
			probed = nil
		}
		if enc.healthy() {
			wait = interval
		} else {
			wait = min(2*wait, max(interval, time.Minute))
		}
		time.Sleep(wait)
	}
}

func runBack(args []string) {
	fs := flag.NewFlagSet("back", flag.ExitOnError)
	listen := fs.String("listen", "", "listen address, host:port or unix:/path")
	domains := fs.String("domains", "", "comma-separated origins requests may name")
	admin := fs.String("admin", "", "loopback address for GET /stats")
	cacheMiB := fs.Int64("cache-mib", 256, "chunk bytes kept; they cost about 1.25x this")
	maxBodyMiB := fs.Int64("max-body-mib", 64, "largest body that is deduplicated")
	inflightMiB := fs.Int64("inflight-mib", 64, "memory for frames being rebuilt; past it the front resends originals, which stream")
	drain := fs.Duration("drain", 30*time.Second, "on SIGTERM, time for open requests to finish")
	memoryMiB := fs.Int64("memory-mib", 0, "Go soft memory limit (0 = cache*1.25 + inflight + 64; GOMEMLIMIT overrides)")
	oomScore := fs.Int("oom-score-adj", 500, "raise oom_score_adj to this so the kernel kills the back before the anytls server (0 = leave)")
	resolve := fs.String("resolve", "", "test only: comma-separated host:port=addr:port origin overrides")
	upstreamCA := fs.String("upstream-ca", "", "test only: extra PEM roots for origins")
	fs.Parse(args)
	if *listen == "" {
		log.Fatal("-listen is required")
	}
	if *memoryMiB == 0 {
		*memoryMiB = *cacheMiB*5/4 + *inflightMiB + 64
	}
	setMemoryLimit(*memoryMiB)
	preferOOMKill(*oomScore)
	up := route{}
	if *resolve != "" {
		up.resolve = map[string]string{}
		for _, pair := range strings.Split(*resolve, ",") {
			from, to, ok := strings.Cut(pair, "=")
			if !ok {
				log.Fatalf("bad -resolve %q", pair)
			}
			up.resolve[from] = to
		}
	}
	if *upstreamCA != "" {
		pem, err := os.ReadFile(*upstreamCA)
		if err != nil {
			log.Fatal(err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			log.Fatalf("no certificates in %s", *upstreamCA)
		}
		up.tls = &tls.Config{RootCAs: roots}
	}
	dec := newDecoder(up, *cacheMiB<<20, *maxBodyMiB<<20)
	dec.inflight.limit = *inflightMiB << 20
	dec.hosts = mustDomains(*domains)
	serveAdmin(*admin, dec.snapshot)

	ln, err := bind(*listen)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Handler: dec, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}
	log.Printf("dedup back on %s for %s", *listen, strings.Join(dec.hosts, ","))
	go func() {
		if err := server.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	// Meanwhile the front sends new requests straight to their origins.
	untilSignal(*drain, func(ctx context.Context) { server.Shutdown(ctx) })
}

// serveAdmin serves GET /stats on addr, if set, and logs changes every minute.
func serveAdmin(addr string, snapshot func() map[string]any) {
	if addr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(snapshot())
		})
		ln, err := bind(addr)
		if err != nil {
			log.Printf("admin: %v", err)
		} else {
			go http.Serve(ln, mux)
		}
	}
	go func() {
		var last string
		for range time.Tick(time.Minute) {
			b, _ := json.Marshal(snapshot())
			if s := string(b); s != last {
				log.Printf("stats %s", s)
				last = s
			}
		}
	}()
}
