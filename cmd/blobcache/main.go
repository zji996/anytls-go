// Command blobcache serves the encrypted file cache on the public egress. It is
// the edge-blob box of the file channel contract (file-channel.md): Caddy hands
// it https://<service>/b/<token>, it verifies the token, and either answers from
// the ciphertext cache on local disk or fetches once from
// GET /internal/blob/<token> with X-Edge-Credential, streaming to the client
// while it writes the cache.
//
//	blobcache serve -listen 127.0.0.1:8090 -origin http://127.0.0.1:8080 \
//	    -dir /var/cache/edge-blob -size 10GiB -reserve 3GiB \
//	    -sign-key-file ... -credential-file ... -socket /run/edge-blob.sock
//
// It starts locked: no key, no reads, no writes, everything passed through. The
// master key arrives over the control socket (ssh egress blobcache unlock
// -socket ... < key) and lives only in memory, mlock'ed when the kernel allows.
// Log lines never carry tokens, keys, file names or the whole of an object key.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: blobcache serve|unlock|status [flags]")
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "serve":
		runServe(os.Args[2:])
	case "unlock":
		if err := runUnlock(os.Args[2:]); err != nil {
			log.Fatalf("unlock: %v", err)
		}
	case "status":
		if err := runStatus(os.Args[2:]); err != nil {
			log.Fatalf("status: %v", err)
		}
	default:
		usage()
	}
}

func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "", "listen address, host:port")
	origin := fs.String("origin", "", "origin base URL, e.g. http://127.0.0.1:8080; the client's Host is kept")
	dir := fs.String("dir", "", "cache directory, one ciphertext file per object")
	size := fs.String("size", "10GiB", "cache size, e.g. 10GiB")
	reserve := fs.String("reserve", "3GiB", "free disk space always left, e.g. 3GiB")
	maxObject := fs.String("max-object", "1GiB", "largest object cached; larger ones only pass through")
	signKeyFile := fs.String("sign-key-file", "", "token signing key, 64 hexadecimal characters")
	credentialFile := fs.String("credential-file", "", "egress credential for the origin, 64 hexadecimal characters")
	socket := fs.String("socket", "", "control unix socket path, created with mode 0600")
	fs.Parse(args)
	if *listen == "" || *origin == "" || *dir == "" || *signKeyFile == "" || *credentialFile == "" {
		log.Fatal("-listen, -origin, -dir, -sign-key-file and -credential-file are required")
	}
	originURL, err := url.Parse(*origin)
	if err != nil || originURL.Scheme == "" || originURL.Host == "" {
		log.Fatalf("bad -origin %q: want http://host:port", *origin)
	}
	capacity, err := parseSize(*size)
	if err != nil {
		log.Fatalf("bad -size: %v", err)
	}
	reserveBytes, err := parseSize(*reserve)
	if err != nil {
		log.Fatalf("bad -reserve: %v", err)
	}
	maxBytes, err := parseSize(*maxObject)
	if err != nil {
		log.Fatalf("bad -max-object: %v", err)
	}
	signKey, err := readHexKeyFile(*signKeyFile)
	if err != nil {
		log.Fatalf("-sign-key-file: %v", err)
	}
	credential, err := readHexKeyFile(*credentialFile)
	if err != nil {
		log.Fatalf("-credential-file: %v", err)
	}
	c, err := newCache(*dir, capacity, reserveBytes)
	if err != nil {
		log.Fatalf("cache: %v", err)
	}
	srv := newServer(originURL, hex.EncodeToString(credential), signKey, c, maxBytes)

	if *socket != "" {
		ln, err := listenControl(*socket)
		if err != nil {
			log.Fatalf("control socket: %v", err)
		}
		defer os.Remove(*socket)
		go func() {
			if err := http.Serve(ln, srv.controlMux()); !errors.Is(err, net.ErrClosed) {
				log.Fatalf("control socket: %v", err)
			}
		}()
	} else {
		log.Printf("blobcache: no -socket, unlock and status are unavailable")
	}

	httpSrv := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	objects, used := c.stats()
	log.Printf("blobcache on %s -> %s, %d bytes cached in %s (%d objects, %d bytes kept), locked", *listen, originURL.Redacted(), capacity, *dir, objects, used)
	go func() {
		if err := httpSrv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	log.Printf("blobcache: shutting down")
	srv.cancel()
	drain, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(drain); err != nil {
		log.Printf("blobcache: shutdown: %v", err)
	}
	c.wipe()
}

func runUnlock(args []string) error {
	fs := flag.NewFlagSet("unlock", flag.ExitOnError)
	socket := fs.String("socket", "", "control unix socket of the running blobcache")
	fs.Parse(args)
	if *socket == "" {
		return errors.New("-socket is required")
	}
	return unlockKey(*socket, os.Stdin)
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	socket := fs.String("socket", "", "control unix socket of the running blobcache")
	fs.Parse(args)
	if *socket == "" {
		return errors.New("-socket is required")
	}
	report, err := fetchStatus(*socket)
	if err != nil {
		return err
	}
	line, err := json.Marshal(report)
	if err != nil {
		return err
	}
	fmt.Println(string(line))
	return nil
}

// parseSize reads a byte count with an optional unit: 1024, 512MiB, 10GiB,
// 1TB. Binary and decimal units are both accepted.
func parseSize(s string) (int64, error) {
	text := strings.TrimSpace(s)
	if text == "" {
		return 0, errors.New("empty size")
	}
	digits := 0
	for digits < len(text) && (text[digits] >= '0' && text[digits] <= '9' || text[digits] == '.') {
		digits++
	}
	value, err := strconv.ParseFloat(text[:digits], 64)
	if err != nil {
		return 0, fmt.Errorf("bad number in %q", text)
	}
	units := map[string]float64{
		"": 1, "b": 1,
		"k": 1e3, "kb": 1e3, "kib": 1 << 10,
		"m": 1e6, "mb": 1e6, "mib": 1 << 20,
		"g": 1e9, "gb": 1e9, "gib": 1 << 30,
		"t": 1e12, "tb": 1e12, "tib": 1 << 40,
	}
	mult, ok := units[strings.ToLower(strings.TrimSpace(text[digits:]))]
	if !ok {
		return 0, fmt.Errorf("unknown unit in %q", text)
	}
	if value < 0 {
		return 0, fmt.Errorf("negative size %q", text)
	}
	return int64(value * mult), nil
}
