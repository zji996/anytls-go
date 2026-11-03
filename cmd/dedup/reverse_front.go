package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// The reverse entry lets an upstream join dedup without intercepting its TLS:
// a client on the LAN asks for
//
//	http://<reverse>/<origin>/<path>?<query>
//
// and the front sends https://<origin>/<path>?<query> through the lane the
// -reverse-auth file names. A request a healthy decoder accepts is framed like
// an intercepted one, and shares its lane, encoder and index; anything else
// (no decoder, a decoder that has not been told about the origin yet, an
// upgrade) goes through the lane's direct proxy, so the request completes
// without dedup.

// reverseFront is the plain HTTP listener of the reverse entry.
type reverseFront struct {
	front *front
	// origins, reloaded on SIGHUP, are the upstreams requests may name.
	origins *domainSet
	// auth names the lane every reverse request uses, as "user:password".
	auth atomic.Pointer[[2]string]
	// allow is the client source networks the entry serves.
	allow []*net.IPNet
}

// reverseConfig collects what runFront read from its flags.
type reverseConfig struct {
	origins, auth string
	allow         string
}

func newReverseFront(f *front, c reverseConfig) (*reverseFront, error) {
	r := &reverseFront{front: f, origins: newDomainSet(nil)}
	if c.origins != "" {
		if err := r.origins.load(c.origins); err != nil {
			return nil, fmt.Errorf("-reverse-origins: %w", err)
		}
	}
	user, pass, err := readAuth(c.auth)
	if err != nil {
		return nil, err
	}
	r.auth.Store(&[2]string{user, pass})
	// The entry is plain HTTP carrying API keys: name its clients.
	if r.allow, err = parseCIDRs(c.allow); err != nil {
		return nil, fmt.Errorf("-reverse-allow is required with -reverse: %w", err)
	}
	return r, nil
}

// start serves the entry on ln. done yields nil once Shutdown has ended it,
// or the error that ended it otherwise.
func (r *reverseFront) start(ln net.Listener) (*http.Server, <-chan error) {
	srv := &http.Server{Handler: r, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 5 * time.Minute}
	done := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		done <- err
	}()
	return srv, done
}

// reload rereads the origins and the auth file on SIGHUP. A file that does not
// parse leaves what is in use as it was.
func (r *reverseFront) reload(c reverseConfig) error {
	var errs []error
	if c.origins != "" {
		if err := r.origins.load(c.origins); err != nil {
			errs = append(errs, fmt.Errorf("reverse origins not reloaded: %w", err))
		} else {
			log.Printf("reverse origins reloaded: %s", r.origins)
		}
	}
	if c.auth != "" {
		if user, pass, err := readAuth(c.auth); err != nil {
			errs = append(errs, fmt.Errorf("reverse auth not reloaded: %w", err))
		} else if old := r.auth.Swap(&[2]string{user, pass}); old == nil || *old != [2]string{user, pass} {
			log.Printf("reverse auth reloaded for user %q", user)
		}
	}
	return errors.Join(errs...)
}

// readAuth reads the "user:password" a reverse request names its lane with.
func readAuth(path string) (user, pass string, err error) {
	if path == "" {
		return "", "", errors.New("-reverse-auth is required with -reverse")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("-reverse-auth: %w", err)
	}
	line, _, _ := strings.Cut(string(b), "\n")
	user, pass, ok := strings.Cut(strings.TrimSpace(line), ":")
	if !ok || user == "" {
		return "", "", fmt.Errorf("-reverse-auth %s: want user:password", path)
	}
	return user, pass, nil
}

// parseCIDRs reads comma-separated networks; a bare address is one host.
func parseCIDRs(s string) ([]*net.IPNet, error) {
	var nets []*net.IPNet
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(entry); err == nil {
			nets = append(nets, n)
			continue
		}
		ip := net.ParseIP(entry)
		if ip == nil {
			return nil, fmt.Errorf("bad network %q", entry)
		}
		bits := 8 * net.IPv6len
		if ip.To4() != nil {
			ip, bits = ip.To4(), 8*net.IPv4len
		}
		nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	if len(nets) == 0 {
		return nil, errors.New("no networks")
	}
	return nets, nil
}

// allows reports whether the client at remoteAddr may use the reverse entry.
func (r *reverseFront) allows(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range r.allow {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// originOf splits "/<origin>/<path>" into the origin and the path to send it.
func originOf(path string) (origin, rest string, ok bool) {
	p := strings.TrimPrefix(path, "/")
	origin, rest, found := strings.Cut(p, "/")
	if !found {
		rest = ""
	}
	if origin == "" || strings.Contains(origin, ":") || strings.Contains(origin, "@") {
		// Port 443 only, and no userinfo: the origin is a bare name.
		return "", "", false
	}
	if _, err := parseDomains(origin); err != nil {
		return "", "", false
	}
	return origin, "/" + rest, true
}

func (r *reverseFront) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	f := r.front
	f.reverseReqs.Add(1)
	if !r.allows(req.RemoteAddr) {
		f.reverseRefused.Add(1)
		log.Printf("reverse: refused %s for %s", req.RemoteAddr, req.URL.Path)
		http.Error(w, "source not allowed", http.StatusForbidden)
		return
	}
	origin, rest, ok := originOf(req.URL.Path)
	if !ok {
		f.reverseRefused.Add(1)
		http.Error(w, "want /<origin>/<path>", http.StatusBadRequest)
		return
	}
	if !r.origins.has(origin) {
		f.reverseRefused.Add(1)
		log.Printf("reverse: %s is not an allowed origin", origin)
		http.Error(w, "origin not allowed", http.StatusMisdirectedRequest)
		return
	}
	auth := r.auth.Load()
	l := f.lanes.get(auth[0], auth[1])
	if l == nil {
		f.reverseRefused.Add(1)
		http.Error(w, "no lane", http.StatusServiceUnavailable)
		return
	}
	// The origin replaces the first path segment, and the Host the reverse
	// entry's own name.
	esc := strings.TrimPrefix(req.URL.EscapedPath(), "/")
	req.URL.Path, req.URL.RawPath = rest, ""
	// Keep the client's escaping (say %2F) where it encodes the same path.
	if _, escRest, found := strings.Cut(esc, "/"); found {
		if decoded, err := url.PathUnescape("/" + escRest); err == nil && decoded == rest {
			req.URL.RawPath = "/" + escRest
		}
	}
	req.Host = origin
	// An upgrade, such as a WebSocket, needs a connection of its own, which
	// the decoder path cannot give a long-lived stream.
	if req.Header.Get("Upgrade") == "" && l.enc.healthy() && l.enc.accepts(origin, f.domains) {
		l.enc.ServeHTTP(w, req)
		return
	}
	f.reverseDirect.Add(1)
	l.direct.ServeHTTP(w, req)
}
