package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/go-kit/kit/log"
	quic "github.com/lucas-clemente/quic-go"
	"github.com/oklog/run"
)

func main() {
	l := log.NewLogfmtLogger(log.NewSyncWriter(os.Stdout))
	l = log.WithPrefix(l, "ts", log.DefaultTimestampUTC)

	var g run.Group

	// proxy code loop
	{
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		g.Add(func() error {
			return loop(l, ctx)
		}, func(error) {
			cancel()
		})
	}

	// signal termination
	{
		sigterm := make(chan os.Signal, 1)
		g.Add(func() error {
			signal.Notify(sigterm, syscall.SIGINT, syscall.SIGTERM)
			if sig, ok := <-sigterm; ok {
				l.Log("msg", "stopping the proxy", "signal", sig.String())
			}
			return nil
		}, func(error) {
			signal.Stop(sigterm)
			close(sigterm)
		})
	}

	err := g.Run()
	if err != nil {
		l.Log("msg", "terminating after error", "err", err)
		os.Exit(1)
	}
}

func loop(l log.Logger, ctx context.Context) error {
	var (
		addr       string
		tlsCert    string
		tlsKey     string
		udpBackend string
	)

	flag.StringVar(&addr, "listen", "127.0.0.1:784", "UDP address to listen on.")
	flag.StringVar(&tlsCert, "cert", "cert.pem", "TLS certificate path.")
	flag.StringVar(&tlsKey, "key", "key.pem", "TLS key path.")
	flag.StringVar(&udpBackend, "udp_backend", "8.8.4.4:53", "UDP of backend server.")

	flag.Parse()

	cert, err := tls.LoadX509KeyPair(tlsCert, tlsKey)
	if err != nil {
		return fmt.Errorf("load certificate: %w", err)
	}

	tls := tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"dq"},
	}

	listener, err := quic.ListenAddr(addr, &tls, nil)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()

	l.Log("msg", "listening for clients", "addr", addr)

	wg := sync.WaitGroup{}

	for {
		session, err := listener.Accept(ctx)
		if err != nil {
			wg.Wait()
			return fmt.Errorf("accept connection: %w", err)
		}

		l := log.With(l, "client", session.RemoteAddr())
		wg.Add(1)
		go func() {
			handleClient(l, ctx, session, udpBackend)
			wg.Done()
		}()
	}

}

func handleClient(l log.Logger, ctx context.Context, session quic.Session, udpBackend string) {
	l.Log("msg", "session accepted")

	wg := sync.WaitGroup{}
	for {
		stream, err := session.AcceptStream(ctx)
		if err != nil {
			break
		}

		l := log.With(l, "stream_id", stream.StreamID())
		l.Log("msg", "stream accepted")

		wg.Add(1)
		go func() {
			err := handleStream(stream, udpBackend)
			if err != nil {
				l.Log("msg", "stream failure", "err", err)
			}
			l.Log("msg", "stream closed")
		}()
	}

	session.CloseWithError(0, "") // TODO: Is this how the session should be closed?
	wg.Done()
	l.Log("msg", "session closed")
}

func handleStream(stream quic.Stream, udpBackend string) error {
	defer stream.Close()

	data, err := ioutil.ReadAll(stream)
	if err != nil {
		return fmt.Errorf("read query: %w", err)
	}

	conn, err := net.Dial("udp", udpBackend)
	if err != nil {
		return fmt.Errorf("connect to backend: %w", err)
	}

	_, err = conn.Write(data)
	if err != nil {
		return fmt.Errorf("send query to backend: %w", err)
	}

	buf := make([]byte, 4096)
	size, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("read response from backend: %w", err)
	}
	buf = buf[:size]

	_, err = stream.Write(buf)
	if err != nil {
		return fmt.Errorf("send response: %w", err)
	}
	return nil
}
