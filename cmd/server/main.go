package main

import (
	"anytls/proxy/padding"
	"anytls/util"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"flag"
	"net"
	"os"
	"time"

	"github.com/sirupsen/logrus"
)

var passwordSha256 []byte

func main() {
	listen := flag.String("l", "0.0.0.0:8443", "server listen port")
	password := flag.String("p", "", "password")
	paddingScheme := flag.String("padding-scheme", "", "padding-scheme")
	fallbackAddr := flag.String("fallback", "127.0.0.1:80", "fallback address for invalid connections")
	flag.Parse()

	if *password == "" {
		logrus.Fatalln("please set password")
	}
	if *paddingScheme != "" {
		b, err := os.ReadFile(*paddingScheme)
		if err != nil {
			logrus.Fatalln(err)
		}
		if padding.UpdatePaddingScheme(b) {
			logrus.Infoln("loaded padding scheme file:", *paddingScheme)
		} else {
			logrus.Errorln("wrong format padding scheme file:", *paddingScheme)
		}
	}

	logLevel, err := logrus.ParseLevel(os.Getenv("LOG_LEVEL"))
	if err != nil {
		logLevel = logrus.InfoLevel
	}
	logrus.SetLevel(logLevel)

	var sum = sha256.Sum256([]byte(*password))
	passwordSha256 = sum[:]

	logrus.Infoln("[Server]", util.ProgramVersionName)
	logrus.Infoln("[Server] Listening TCP", *listen)
	if *fallbackAddr != "" {
		logrus.Infoln("[Server] Fallback", *fallbackAddr)
	}

	listener, err := net.Listen("tcp", *listen)
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
	server := NewMyServer(tlsConfig, *fallbackAddr)

	for {
		c, err := listener.Accept()
		if err != nil {
			logrus.Fatalln("accept:", err)
		}
		go handleTcpConnection(ctx, c, server)
	}
}
