package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// domainList matches names equal to or under any of its domains, the way
// x509 name constraints do.
type domainList []string

func parseDomains(s string) (domainList, error) {
	var d domainList
	for _, name := range strings.Split(s, ",") {
		name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
		if name == "" {
			continue
		}
		if strings.ContainsAny(name, "*/: ") || net.ParseIP(name) != nil {
			return nil, fmt.Errorf("bad domain %q", name)
		}
		d = append(d, name)
	}
	if len(d) == 0 {
		return nil, errors.New("no domains")
	}
	return d, nil
}

func (d domainList) match(name string) bool {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	for _, domain := range d {
		if name == domain || strings.HasSuffix(name, "."+domain) {
			return true
		}
	}
	return false
}

// hostname strips the port from a Host header.
func hostname(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

const (
	caCertFile = "ca.pem"
	caKeyFile  = "ca.key"
)

// createCA writes a CA that can only certify names under domains, so its key
// cannot impersonate any other site even if it leaks.
func createCA(dir string, domains domainList) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:                serial(),
		Subject:                     pkix.Name{CommonName: "anytls-dedup CA " + host, Organization: []string{"anytls-dedup"}},
		NotBefore:                   now.Add(-time.Hour),
		NotAfter:                    now.AddDate(10, 0, 0),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         domains,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	keyPath := filepath.Join(dir, caKeyFile)
	f, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := errors.Join(pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), f.Close()); err != nil {
		os.Remove(keyPath)
		return err
	}
	return os.WriteFile(filepath.Join(dir, caCertFile), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

// authority issues short-lived leaf certificates for intercepted names.
type authority struct {
	cert    *x509.Certificate
	key     crypto.Signer
	leafKey *ecdsa.PrivateKey

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

const leafLifetime = 7 * 24 * time.Hour

func loadAuthority(dir string) (*authority, error) {
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, caCertFile), filepath.Join(dir, caKeyFile))
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	key, ok := pair.PrivateKey.(crypto.Signer)
	if !ok || !cert.IsCA {
		return nil, errors.New("not a CA key pair")
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return &authority{cert: cert, key: key, leafKey: leafKey, leaves: map[string]*tls.Certificate{}}, nil
}

// leaf returns a certificate for name, reissued a day before it expires.
func (a *authority) leaf(name string) (*tls.Certificate, error) {
	name = strings.ToLower(name)
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.leaves[name]; ok && time.Now().Before(c.Leaf.NotAfter.Add(-24*time.Hour)) {
		return c, nil
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(leafLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &a.leafKey.PublicKey, a.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	c := &tls.Certificate{Certificate: [][]byte{der, a.cert.Raw}, PrivateKey: a.leafKey, Leaf: leaf}
	a.leaves[name] = c
	return c, nil
}

// check fails unless every domain gets a leaf that verifies against the CA,
// e.g. because the CA's name constraints do not cover it.
func (a *authority) check(domains domainList) error {
	roots := x509.NewCertPool()
	roots.AddCert(a.cert)
	for _, d := range domains {
		c, err := a.leaf(d)
		if err != nil {
			return err
		}
		if _, err := c.Leaf.Verify(x509.VerifyOptions{DNSName: d, Roots: roots}); err != nil {
			return fmt.Errorf("CA cannot certify %s: %w", d, err)
		}
	}
	return nil
}
