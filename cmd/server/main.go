package main

import (
	"anytls/proxy/padding"
	"anytls/util"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

var passwordSha256 []byte

type serverConfig struct {
	listen        string
	password      string
	paddingScheme string
	fallbackAddr  string
}

func envOrDefault(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func loadPassword(password, passwordFile string) (string, error) {
	if password != "" && passwordFile != "" {
		return "", fmt.Errorf("set only one of password and password-file")
	}
	if passwordFile != "" {
		contents, err := os.ReadFile(passwordFile)
		if err != nil {
			return "", fmt.Errorf("read password file: %w", err)
		}
		password = strings.TrimSuffix(string(contents), "\n")
		password = strings.TrimSuffix(password, "\r")
	}
	if password == "" {
		return "", fmt.Errorf("password is required")
	}
	if strings.ContainsAny(password, "\r\n") {
		return "", fmt.Errorf("password must not contain newlines")
	}
	return password, nil
}

func parseServerConfig(args []string) (serverConfig, error) {
	flags := flag.NewFlagSet("anytls-server", flag.ContinueOnError)
	listen := flags.String("l", envOrDefault("ANYTLS_LISTEN", "0.0.0.0:8443"), "server listen port")
	password := flags.String("p", "", "password (prefer -password-file for services)")
	passwordFile := flags.String("password-file", os.Getenv("ANYTLS_PASSWORD_FILE"), "file containing the password")
	paddingScheme := flags.String("padding-scheme", os.Getenv("ANYTLS_PADDING_SCHEME"), "padding-scheme")
	fallbackAddr := flags.String("fallback", envOrDefault("ANYTLS_FALLBACK", "127.0.0.1:80"), "fallback address for invalid connections")
	if err := flags.Parse(args); err != nil {
		return serverConfig{}, err
	}
	if flags.NArg() != 0 {
		return serverConfig{}, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	resolvedPassword, err := loadPassword(*password, *passwordFile)
	if err != nil {
		return serverConfig{}, err
	}
	return serverConfig{
		listen:        *listen,
		password:      resolvedPassword,
		paddingScheme: *paddingScheme,
		fallbackAddr:  *fallbackAddr,
	}, nil
}

func main() {
	config, err := parseServerConfig(os.Args[1:])
	if err != nil {
		logrus.Fatalln(err)
	}
	if config.paddingScheme != "" {
		b, err := os.ReadFile(config.paddingScheme)
		if err != nil {
			logrus.Fatalln(err)
		}
		if padding.UpdatePaddingScheme(b) {
			logrus.Infoln("loaded padding scheme file:", config.paddingScheme)
		} else {
			logrus.Errorln("wrong format padding scheme file:", config.paddingScheme)
		}
	}

	logLevel, err := logrus.ParseLevel(os.Getenv("LOG_LEVEL"))
	if err != nil {
		logLevel = logrus.InfoLevel
	}
	logrus.SetLevel(logLevel)

	var sum = sha256.Sum256([]byte(config.password))
	passwordSha256 = sum[:]

	logrus.Infoln("[Server]", util.ProgramVersionName)
	logrus.Infoln("[Server] Listening TCP", config.listen)
	if config.fallbackAddr != "" {
		logrus.Infoln("[Server] Fallback", config.fallbackAddr)
	}

	listener, err := net.Listen("tcp", config.listen)
	if err != nil {
		logrus.Fatalln("listen server tcp:", err)
	}

	tlsCert, err := util.GenerateKeyPair(time.Now, "")
	if err != nil {
		logrus.Fatalln("generate TLS certificate:", err)
	}
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{*tlsCert},
	}

	ctx := context.Background()
	server := NewMyServer(tlsConfig, config.fallbackAddr)

	for {
		c, err := listener.Accept()
		if err != nil {
			logrus.Fatalln("accept:", err)
		}
		go handleTcpConnection(ctx, c, server)
	}
}
