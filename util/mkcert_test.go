package util

import (
	"crypto/ecdsa"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"
)

func TestGenerateKeyPairUsesECDSA(t *testing.T) {
	certificate, err := GenerateKeyPair(time.Now, "localhost")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := certificate.PrivateKey.(*ecdsa.PrivateKey); !ok {
		t.Fatalf("private key type = %T, want ECDSA", certificate.PrivateKey)
	}
}

func TestTLSClientSessionCacheResumes(t *testing.T) {
	certificate, err := GenerateKeyPair(time.Now, "localhost")
	if err != nil {
		t.Fatal(err)
	}
	serverConfig := &tls.Config{Certificates: []tls.Certificate{*certificate}}
	clientConfig := &tls.Config{
		ServerName:         "localhost",
		InsecureSkipVerify: true,
		ClientSessionCache: tls.NewLRUClientSessionCache(4),
	}

	resumed, err := runTLSHandshake(serverConfig, clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	if resumed {
		t.Fatal("first TLS connection unexpectedly resumed")
	}
	resumed, err = runTLSHandshake(serverConfig, clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed {
		t.Fatal("second TLS connection did not resume")
	}
}

func BenchmarkTLSHandshakeFull(b *testing.B) {
	benchmarkTLSHandshake(b, false)
}

func BenchmarkTLSHandshakeResumed(b *testing.B) {
	benchmarkTLSHandshake(b, true)
}

func benchmarkTLSHandshake(b *testing.B, resume bool) {
	certificate, err := GenerateKeyPair(time.Now, "localhost")
	if err != nil {
		b.Fatal(err)
	}
	serverConfig := &tls.Config{Certificates: []tls.Certificate{*certificate}}
	clientConfig := &tls.Config{ServerName: "localhost", InsecureSkipVerify: true}
	if resume {
		clientConfig.ClientSessionCache = tls.NewLRUClientSessionCache(4)
		if _, err := runTLSHandshake(serverConfig, clientConfig); err != nil {
			b.Fatal(err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resumed, err := runTLSHandshake(serverConfig, clientConfig)
		if err != nil {
			b.Fatal(err)
		}
		if resumed != resume {
			b.Fatalf("resumed = %v, want %v", resumed, resume)
		}
	}
}

func runTLSHandshake(serverConfig, clientConfig *tls.Config) (bool, error) {
	clientRaw, serverRaw := net.Pipe()
	server := tls.Server(serverRaw, serverConfig)
	client := tls.Client(clientRaw, clientConfig)
	serverErr := make(chan error, 1)
	go func() {
		defer serverRaw.Close()
		if err := server.Handshake(); err != nil {
			serverErr <- err
			return
		}
		_, err := server.Write([]byte{1})
		serverErr <- err
	}()
	defer clientRaw.Close()
	if err := client.Handshake(); err != nil {
		return false, err
	}
	if _, err := io.ReadFull(client, make([]byte, 1)); err != nil {
		return false, err
	}
	if err := <-serverErr; err != nil {
		return false, err
	}
	return client.ConnectionState().DidResume, nil
}
