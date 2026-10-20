//go:build go1.27

// The back's TLS handshakes stand in for the clients' (Sub2API's go.mod says
// go 1.27), so it takes that release's defaults, e.g. for signature algorithms
// and key exchange groups, instead of those of this module's go line. Build
// release binaries with a toolchain at least that new.

//go:debug default=go1.27
package main
