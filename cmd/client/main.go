package main

import (
	"anytls/proxy"
	"anytls/util"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	M "github.com/sagernet/sing/common/metadata"
	"github.com/sirupsen/logrus"
)

var passwordSha256 []byte

func main() {
	listen := flag.String("l", "127.0.0.1:1080", "socks5 listen port")
	serverAddr := flag.String("s", "", "Server address or anytls:// link")
	sni := flag.String("sni", "", "Server Name Indication")
	insecure := flag.Bool("insecure", true, "Allow insecure TLS connection")
	password := flag.String("p", "", "Password")
	passwordFile := flag.String("password-file", "", "File containing the password")
	minIdleSession := flag.Int("m", 5, "Reserved min idle session")
	prewarm := flag.Int("prewarm", 0, "Pre-create idle sessions for lower first-request latency")
	probe := flag.Bool("probe", false, "Run a one-shot end-to-end AnyTLS health probe")
	flag.Parse()

	if serverURL, err := url.Parse(*serverAddr); err == nil {
		if serverURL.Scheme == "anytls" {
			*serverAddr = withDefaultPort(serverURL.Host, "443")
			if serverURL.User != nil {
				*password = serverURL.User.String()
			}
			query := serverURL.Query()
			*sni = query.Get("sni")
			if rawInsecure := query.Get("insecure"); rawInsecure != "" {
				if parsed, err := strconv.ParseBool(rawInsecure); err == nil {
					*insecure = parsed
				} else if rawInsecure == "1" {
					*insecure = true
				} else if rawInsecure == "0" {
					*insecure = false
				}
			}
		}
	}

	if *serverAddr == "" {
		logrus.Fatalln("please set -s server adreess")
	}

	if *password != "" && *passwordFile != "" {
		logrus.Fatalln("set only one of -p and -password-file")
	}
	if *passwordFile != "" {
		contents, err := os.ReadFile(*passwordFile)
		if err != nil {
			logrus.Fatalln("read password file:", err)
		}
		*password = strings.TrimSuffix(strings.TrimSuffix(string(contents), "\n"), "\r")
	}
	if *password == "" {
		logrus.Fatalln("please set -p or -password-file")
	}
	if strings.ContainsAny(*password, "\r\n") {
		logrus.Fatalln("password must not contain newlines")
	}

	if _, _, err := net.SplitHostPort(*serverAddr); err != nil {
		logrus.Fatalln("error server address:", *serverAddr, err)
	}

	logLevel, err := logrus.ParseLevel(os.Getenv("LOG_LEVEL"))
	if err != nil {
		logLevel = logrus.InfoLevel
	}
	logrus.SetLevel(logLevel)

	var sum = sha256.Sum256([]byte(*password))
	passwordSha256 = sum[:]

	logrus.Infoln("[Client]", util.ProgramVersionName)

	// You can only use `InsecureSkipVerify` by default in the sample client; it is not recommended for use in production code.
	tlsConfig := &tls.Config{
		ServerName:         *sni,
		InsecureSkipVerify: *insecure,
		ClientSessionCache: tls.NewLRUClientSessionCache(64),
	}
	if tlsConfig.ServerName == "" {
		// disable the SNI
		tlsConfig.ServerName = "127.0.0.1"
	} else if _, err := netip.ParseAddr(tlsConfig.ServerName); err == nil {
		// RFC 6066 forbids literal IP addresses in SNI. Leave ServerName
		// empty only when verification is disabled, matching URI semantics.
		if tlsConfig.InsecureSkipVerify {
			tlsConfig.ServerName = ""
		}
	}

	path := strings.TrimSpace(os.Getenv("TLS_KEY_LOG"))
	if path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
		if err == nil {
			tlsConfig.KeyLogWriter = f
		}
	}

	ctx := context.Background()
	client := NewMyClient(ctx, func(ctx context.Context) (net.Conn, error) {
		conn, err := proxy.SystemDialer.DialContext(ctx, "tcp", *serverAddr)
		if err != nil {
			return nil, err
		}
		conn = tls.Client(conn, tlsConfig)
		return conn, nil
	}, *minIdleSession)
	if *probe {
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		defer client.Close()
		if err := runHealthProbe(probeCtx, client); err != nil {
			logrus.Fatalln("health probe failed:", err)
		}
		logrus.Infoln("health probe succeeded")
		return
	}

	logrus.Infoln("[Client] socks5/http", *listen, "=>", *serverAddr)
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		logrus.Fatalln("listen socks5 tcp:", err)
	}
	if *prewarm > 0 {
		go func() {
			if err := client.Prewarm(ctx, *prewarm); err != nil {
				logrus.Warnln("prewarm sessions:", err)
			}
		}()
	}

	for {
		c, err := listener.Accept()
		if err != nil {
			logrus.Fatalln("accept:", err)
		}
		go handleTcpConnection(ctx, c, client)
	}
}

type proxyCreator interface {
	CreateProxy(ctx context.Context, destination M.Socksaddr) (net.Conn, error)
}

func runHealthProbe(ctx context.Context, client proxyCreator) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen for echo target: %w", err)
	}
	defer listener.Close()
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	payload := make([]byte, 32)
	if _, err = rand.Read(payload); err != nil {
		return fmt.Errorf("create probe payload: %w", err)
	}
	echoErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			echoErr <- acceptErr
			return
		}
		defer conn.Close()
		if deadline, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(deadline)
		}
		request := make([]byte, len(payload))
		if _, acceptErr = io.ReadFull(conn, request); acceptErr == nil {
			_, acceptErr = io.Copy(conn, bytes.NewReader(request))
		}
		echoErr <- acceptErr
	}()

	conn, err := client.CreateProxy(ctx, M.SocksaddrFromNet(listener.Addr()))
	if err != nil {
		return fmt.Errorf("create AnyTLS probe stream: %w", err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err = io.Copy(conn, bytes.NewReader(payload)); err != nil {
		return fmt.Errorf("write probe payload: %w", err)
	}
	response := make([]byte, len(payload))
	if _, err = io.ReadFull(conn, response); err != nil {
		return fmt.Errorf("read probe response: %w", err)
	}
	if !bytes.Equal(response, payload) {
		return fmt.Errorf("probe payload mismatch")
	}
	select {
	case err = <-echoErr:
		if err != nil {
			return fmt.Errorf("echo target: %w", err)
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func withDefaultPort(address string, defaultPort string) string {
	if address == "" {
		return address
	}
	if _, _, err := net.SplitHostPort(address); err == nil {
		return address
	}
	address = strings.TrimPrefix(strings.TrimSuffix(address, "]"), "[")
	return net.JoinHostPort(address, defaultPort)
}
