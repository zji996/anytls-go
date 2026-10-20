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
	listen := fs.String("listen", "127.0.0.1:10860", "SOCKS5/HTTP proxy listen address")
	via := fs.String("via", "", "SOCKS5 proxy toward the exit, e.g. socks5://127.0.0.1:10808")
	decoder := fs.String("decoder", "http://"+dedupHost, "decoder URL, reached through -via")
	domains := fs.String("domains", "", "comma-separated domains whose TLS is intercepted")
	caDir := fs.String("ca-dir", "", "directory with ca.pem and ca.key")
	admin := fs.String("admin", "", "loopback address for GET /stats")
	indexMiB := fs.Int64("index-mib", 512, "content indexed as held by the decoder; keep below its -cache-mib")
	maxBodyMiB := fs.Int64("max-body-mib", 64, "largest body that is deduplicated; larger ones pass through")
	inflightMiB := fs.Int64("inflight-mib", 128, "memory for bodies being deduplicated; others pass through")
	compressFlag := fs.String("compress", "auto", "CPU for content the decoder lacks: auto, huffman (cheaper) or off (cache only)")
	probe := fs.Duration("probe-interval", 15*time.Second, "decoder health check interval")
	fs.Parse(args)
	if *via == "" || *caDir == "" {
		log.Fatal("-via and -ca-dir are required")
	}
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

	var user, pass string
	if viaURL.User != nil {
		user = viaURL.User.Username()
		pass, _ = viaURL.User.Password()
	}
	socksClient := socks.NewClient(N.SystemDialer, M.ParseSocksaddr(viaURL.Host), socks.Version5, user, pass)
	dial := func(ctx context.Context, dest M.Socksaddr) (net.Conn, error) {
		return socksClient.DialContext(ctx, N.NetworkTCP, dest)
	}

	enc := newEncoder(route{url: decURL, via: viaURL}, *indexMiB<<20, *maxBodyMiB<<20)
	enc.maxInflight = *inflightMiB << 20
	if enc.compress, err = parseCompression(*compressFlag); err != nil {
		log.Fatal(err)
	}
	direct := newProxy(route{via: viaURL})
	enc.origin = direct.Transport
	f := newFront(list, ca, dial, enc, direct)

	go func() {
		for {
			enc.probe(context.Background(), decURL)
			time.Sleep(*probe)
		}
	}()
	serveAdmin(*admin, f.snapshot)

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("dedup front on %s for %s via %s -> %s", *listen, strings.Join(list, ","), viaURL.Host, decURL)
	log.Fatal(f.serve(ln))
}

func runBack(args []string) {
	fs := flag.NewFlagSet("back", flag.ExitOnError)
	listen := fs.String("listen", "", "listen address, host:port or unix:/path")
	domains := fs.String("domains", "", "comma-separated origins requests may name")
	admin := fs.String("admin", "", "loopback address for GET /stats")
	cacheMiB := fs.Int64("cache-mib", 512, "chunk cache size")
	maxBodyMiB := fs.Int64("max-body-mib", 64, "largest body that is deduplicated")
	resolve := fs.String("resolve", "", "test only: comma-separated host:port=addr:port origin overrides")
	upstreamCA := fs.String("upstream-ca", "", "test only: extra PEM roots for origins")
	fs.Parse(args)
	if *listen == "" {
		log.Fatal("-listen is required")
	}
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
	dec.hosts = mustDomains(*domains)
	serveAdmin(*admin, dec.snapshot)

	network, address := "tcp", *listen
	if path, ok := strings.CutPrefix(*listen, "unix:"); ok {
		network, address = "unix", path
		os.Remove(path)
	}
	ln, err := net.Listen(network, address)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Handler: dec, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}
	log.Printf("dedup back on %s for %s", *listen, strings.Join(dec.hosts, ","))
	log.Fatal(server.Serve(ln))
}

// serveAdmin serves GET /stats on addr, if set, and logs changes every minute.
func serveAdmin(addr string, snapshot func() map[string]any) {
	if addr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(snapshot())
		})
		go func() { log.Fatal(http.ListenAndServe(addr, mux)) }()
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
