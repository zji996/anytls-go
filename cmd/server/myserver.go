package main

import (
	"crypto/tls"
)

type myServer struct {
	tlsConfig    *tls.Config
	fallbackAddr string
	dedupBackend string
}

func NewMyServer(tlsConfig *tls.Config, fallbackAddr, dedupBackend string) *myServer {
	s := &myServer{
		tlsConfig:    tlsConfig,
		fallbackAddr: fallbackAddr,
		dedupBackend: dedupBackend,
	}
	return s
}
