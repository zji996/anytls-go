package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// domainsFileDecoder is a decoder whose origin list lives in a file, as
// runBack sets it up with -domains-file.
func domainsFileDecoder(t *testing.T, hosts ...string) (*decoder, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "domains")
	writeFile(t, path, strings.Join(hosts, "\n")+"\n")
	dec := newDecoder(route{}, 1<<20, 1<<20)
	dec.domains = newDomainSet(nil)
	dec.hosts = domainList{hosts[0]}
	if err := dec.domains.load(path); err != nil {
		t.Fatal(err)
	}
	return dec, path
}

// health asks the decoder's /health, as the encoder's probe does.
func health(t *testing.T, dec *decoder) (int, http.Header) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://"+dedupHost+"/health", nil)
	req.Host = dedupHost
	w := httptest.NewRecorder()
	dec.ServeHTTP(w, req)
	resp := w.Result()
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, resp.Header
}

// ask sends a request naming host and returns its status. Only a refusal is
// answered here without reaching an origin.
func ask(t *testing.T, dec *decoder, host string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://"+host+"/v1/models", nil)
	req.Host = host
	w := httptest.NewRecorder()
	dec.ServeHTTP(w, req)
	resp := w.Result()
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// refused reports whether a request for host is refused, without sending one
// anywhere.
func refused(dec *decoder, host string) bool { return !dec.allowed(host) }

// A reload adds and removes origins without a restart, and a file that does
// not parse keeps the list in use.
func TestBackDomainsFileReloads(t *testing.T) {
	dec, path := domainsFileDecoder(t, "example.com")
	if refused(dec, "example.com") {
		t.Fatal("a listed origin is refused")
	}
	if status := ask(t, dec, "late.test"); status != http.StatusForbidden {
		t.Fatalf("an unlisted origin answered %d, want 403", status)
	}
	writeFile(t, path, "# upstreams\nexample.com\nlate.test\n")
	if err := dec.domains.load(path); err != nil {
		t.Fatal(err)
	}
	if refused(dec, "late.test") {
		t.Fatal("an origin added without a restart is still refused")
	}
	writeFile(t, path, "late.test\n")
	if err := dec.domains.load(path); err != nil {
		t.Fatal(err)
	}
	if status := ask(t, dec, "example.com"); status != http.StatusForbidden {
		t.Fatalf("a removed origin answered %d, want 403", status)
	}
	// A broken file leaves the list in use.
	writeFile(t, path, "*.bad.test\n")
	if err := dec.domains.load(path); err == nil {
		t.Fatal("a wildcard origin was accepted")
	}
	if refused(dec, "late.test") {
		t.Fatal("a broken file dropped the list in use")
	}
	if dec.hostList() == nil || len(dec.hostList()) != 1 {
		t.Fatalf("the list in use is %v", dec.hostList())
	}
}

// /health advertises the list, so a front frames only what the decoder takes.
func TestBackDomainsFileIsAdvertisedOnHealth(t *testing.T) {
	dec, path := domainsFileDecoder(t, "example.com", "sub.example.com")
	status, hdr := health(t, dec)
	if status != http.StatusNoContent {
		t.Fatalf("health answered %d", status)
	}
	if got := hdr.Get(hdrDomains); got != "example.com,sub.example.com" {
		t.Fatalf("%s is %q", hdrDomains, got)
	}
	writeFile(t, path, "example.com\napi.deepseek.com\n")
	if err := dec.domains.load(path); err != nil {
		t.Fatal(err)
	}
	if _, reloaded := health(t, dec); reloaded.Get(hdrDomains) != "example.com,api.deepseek.com" {
		t.Fatalf("after the reload %s is %q", hdrDomains, reloaded.Get(hdrDomains))
	} else {
		hdr = reloaded
	}
	// An encoder reading that header admits exactly the advertised origins.
	enc := newEncoder(route{}, 1<<20, 1<<20)
	enc.learnDomains(hdr)
	if !enc.accepts("api.deepseek.com", nil) || !enc.accepts("sub.example.com", nil) || enc.accepts("other.test", nil) {
		t.Fatalf("the encoder's view of %q is wrong", hdr.Get(hdrDomains))
	}
}

// A decoder given -domains keeps its list, and /health still advertises it;
// one with no list at all advertises none, which a front reads as "older
// decoder".
func TestBackHealthAdvertisesOnlyWhatWasConfigured(t *testing.T) {
	dec := newDecoder(route{}, 1<<20, 1<<20)
	dec.hosts = domainList{"example.com"}
	if _, hdr := health(t, dec); hdr.Get(hdrDomains) != "example.com" {
		t.Fatalf("-domains is not advertised: %q", hdr.Get(hdrDomains))
	}
	open := newDecoder(route{}, 1<<20, 1<<20)
	if _, hdr := health(t, open); hdr[hdrDomains] != nil {
		t.Fatalf("a list-less decoder advertised %q", hdr.Get(hdrDomains))
	}
}

// A cancelled or failed load of the file keeps the list: -domains-file and
// -domains are exclusive, and a missing file is fatal at start.
func TestBackDomainsFileRejectsMistakes(t *testing.T) {
	if _, err := parseDomainFile("example.com:443\n"); err == nil {
		t.Fatal("a port in the list was accepted")
	}
	if _, err := parseDomainFile("10.0.0.1\n"); err == nil {
		t.Fatal("an address in the list was accepted")
	}
	list, err := parseDomainFile("\n# only comments\n")
	if err != nil || len(list) != 0 {
		t.Fatalf("an empty file: %v, %v", list, err)
	}
	set := newDomainSet(nil)
	if err := set.load(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a missing file was accepted")
	}
	set.set(domainList{"kept.example"})
	if err := set.load(filepath.Join(t.TempDir(), "missing")); err == nil || !set.match("kept.example") {
		t.Fatal("a failed load dropped the list in use")
	}
	if _, err := os.Stat(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("the test expected a missing file")
	}
}

// The loader the flags use: -domains-file wins over -domains, both at once is
// a mistake, and an unreadable file stops the start.
func TestBackDomainsFileFlags(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "domains")
	writeFile(t, path, "example.com\n")
	dec := newDecoder(route{}, 1<<20, 1<<20)
	dec.domains = newDomainSet(nil)
	if err := dec.domains.load(path); err != nil {
		t.Fatal(err)
	}
	if err := loadBackDomains(dec, "", ""); err == nil {
		t.Fatal("a back with neither -domains nor -domains-file was accepted")
	}
	if err := loadBackDomains(dec, "example.com", path); err == nil {
		t.Fatal("-domains and -domains-file together were accepted")
	}
	if err := loadBackDomains(dec, "", path); err != nil {
		t.Fatal(err)
	}
	if !dec.allowed("sub.example.com") || dec.allowed("other.test") {
		t.Fatalf("the file's list is not in force: %v", dec.hostList())
	}
	fixed := newDecoder(route{}, 1<<20, 1<<20)
	if err := loadBackDomains(fixed, "example.com,other.test", ""); err != nil {
		t.Fatal(err)
	}
	if !fixed.allowed("other.test") || fixed.domains != nil {
		t.Fatalf("-domains did not set a fixed list: %v", fixed.hostList())
	}
	if err := loadBackDomains(newDecoder(route{}, 1<<20, 1<<20), "", filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing -domains-file was accepted")
	}
	if err := loadBackDomains(newDecoder(route{}, 1<<20, 1<<20), "*.bad", ""); err == nil {
		t.Fatal("a wildcard -domains was accepted")
	}
}
