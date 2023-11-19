package wftp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"
)

// Peer represents a single peer in the network. A peer may contain multiple
// Connections, each pointing to a different peer.
type Peer struct {
	logger   *slog.Logger
	secret   []byte
	nickname string
	frontend FrontendHandler

	connMan    *ConnectionManager
	activeConn *Connection
}

// PeerOpts contains options for creating a new peer.
type PeerOpts struct {
	// Nickname is the nickname of the peer. If not provided, the hostname
	// will be used.
	Nickname string
	// Secret is the secret used to authenticate the peer. If not provided, then
	// authentication will not be required.
	Secret []byte
	// Logger is the logger to use for the peer. If not provided, then the
	// default logger will be used.
	Logger *slog.Logger
	// Frontend is the frontend handler to use for the peer. If not provided,
	// no frontend will be used.
	Frontend FrontendHandler
}

// NewPeer creates a new peer.
func NewPeer(opts PeerOpts) (*Peer, error) {
	if opts.Nickname == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("could not get hostname: %v", err)
		}
		opts.Nickname = hostname
	}

	peer := &Peer{
		connMan:  newConnectionManager(),
		secret:   opts.Secret,
		logger:   opts.Logger,
		frontend: opts.Frontend,
		nickname: opts.Nickname,
	}

	return peer, nil
}

func (p *Peer) Listen(ctx context.Context, addr string) error {
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return fmt.Errorf("could not resolve listen address: %v", err)
	}

	listenConn, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		return fmt.Errorf("could not listen on address %s: %v", addr, err)
	}

	var wg sync.WaitGroup
	defer wg.Wait()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	wg.Add(1)
	go func() {
		defer wg.Done()

		<-ctx.Done()
		listenConn.Close()
	}()

	for {
		netConn, err := listenConn.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("could not accept connection: %v", err)
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer netConn.Close()

			conn := newConnection(p, netConn)

			if err := netConn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				conn.logger.Error(
					"could not set deadline on connection",
					"error", err)
				return
			}

			p.connMan.add(conn)

			if err := conn.handleConn(ctx); err != nil {
				conn.logger.Error(
					"connection error",
					"error", err)
				return
			}
		}()
	}
}

func (p *Peer) emitFrontend(ev FrontendEvent) {
	if p.frontend == nil {
		return
	}

	p.frontend.HandleEvent(ev)
}
