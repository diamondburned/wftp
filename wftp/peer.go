package wftp

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
)

// Peer represents a single peer in the network. A peer may contain multiple
// Connections, each pointing to a different peer.
type Peer struct {
	opts       Opts
	logger     *slog.Logger
	connMan    *ConnectionManager
	filesystem billy.Filesystem

	wg sync.WaitGroup

	ctx        context.Context
	cancel     context.CancelCauseFunc
	activeConn *Connection
}

// Opts contains options for creating a new peer.
type Opts struct {
	// CurrentDir is the current directory of the peer. If not provided, then
	// the current working directory will be used.
	CurrentDir string
	// Nickname is the nickname of the peer. If not provided, the hostname
	// will be used.
	Nickname string
	// Secret is the secret used to authenticate the peer. If not provided, then
	// authentication will not be required.
	Secret []byte
	// Logger is the logger to use for the peer. If not provided, then the
	// default logger will be used.
	Logger *slog.Logger
	// ListenConfig is the listen config to use for the peer. If not provided,
	// then the default listen config will be used.
	ListenConfig *net.ListenConfig
}

// NewPeer creates a new peer.
func NewPeer(opts Opts) (*Peer, error) {
	if opts.CurrentDir == "" {
		var err error
		opts.CurrentDir, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("could not get current directory: %v", err)
		}
	}

	if opts.Nickname == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("could not get hostname: %v", err)
		}
		opts.Nickname = hostname
	}

	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	if opts.ListenConfig == nil {
		opts.ListenConfig = &net.ListenConfig{}
	}

	peer := &Peer{
		opts:       opts,
		logger:     opts.Logger.With("nickname", opts.Nickname),
		connMan:    newConnectionManager(),
		filesystem: osfs.New(opts.CurrentDir),
	}

	return peer, nil
}

// Close closes the peer.
func (p *Peer) Close() error {
	p.cancel(nil)
	p.wg.Wait()
	if err := context.Cause(p.ctx); err != p.ctx.Err() {
		return err
	}
	return nil
}

// StartListening starts listening for connections on the given address.
// It returns the address that the peer is listening on.
// If StartListening has already been called, then the function panics.
func (p *Peer) StartListening(ctx context.Context, addr string) (string, error) {
	if p.ctx != nil {
		panic("peer is already listening")
	}

	listenConn, err := p.opts.ListenConfig.Listen(ctx, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("could not listen on address %s: %v", addr, err)
	}

	// Spawn in our own context.
	//
	// This is intentional! The context passed in is only meant to be used for
	// the lifetime of the function call. We want to keep listening for
	// connections until the peer is closed (through p.Close()).
	p.ctx, p.cancel = context.WithCancelCause(context.Background())

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()

		<-p.ctx.Done()
		listenConn.Close()
	}()

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer p.cancel(nil)

		for {
			netConn, err := listenConn.Accept()
			if err != nil {
				p.logger.Error(
					"could not accept connection",
					"error", err)

				p.cancel(err)
				return
			}

			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				defer netConn.Close()

				peer := (*peerPrivate)(p)
				conn := newConnection(netConn, peer, p.logger)

				p.connMan.add(conn)
				defer p.connMan.remove(conn)

				conn.handle(p.ctx, serverConnection)

				if err := netConn.Close(); err != nil {
					p.logger.Error(
						"could not close connection",
						"error", err)
				}
			}()
		}
	}()

	return listenConn.Addr().String(), nil
}

type peerPrivate Peer

func (p *peerPrivate) Filesystem() billy.Filesystem {
	return p.filesystem
}

func (p *peerPrivate) Nickname() string {
	return p.opts.Nickname
}

func (p *peerPrivate) AddNick(conn *Connection, nick string) bool {
	return p.connMan.addNick(conn, nick)
}

func (p *peerPrivate) CompareSecret(got []byte) bool {
	return bytes.Equal(p.opts.Secret, got)
}
