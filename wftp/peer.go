package wftp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"

	"github.com/SierraSoftworks/multicast/v2"
	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	"libdb.so/cpsc-471-assignment/wftp/message"
)

// ConnectionMessage represents a message received from a connection.
type ConnectionMessage struct {
	Connection *Connection
	Message    message.Message
}

// Peer represents a single peer in the network. A peer may contain multiple
// Connections, each pointing to a different peer.
type Peer struct {
	Opts

	// Recv is a channel that receives messages from connections.
	// Not all messages will be useful, but some may be.
	Recv *multicast.Channel[ConnectionMessage]

	wg         sync.WaitGroup
	recvClosed atomic.Bool

	logger     *slog.Logger
	conns      *connectionManager
	filesystem billy.Filesystem
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
	// Dialer is the dialer to use for the peer. If not provided, then the
	// default dialer will be used.
	Dialer *net.Dialer
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

	if opts.Dialer == nil {
		opts.Dialer = &net.Dialer{}
	}

	if opts.ListenConfig == nil {
		opts.ListenConfig = &net.ListenConfig{}
	}

	peer := &Peer{
		Opts:       opts,
		Recv:       multicast.New[ConnectionMessage](),
		logger:     opts.Logger,
		conns:      newConnectionManager(),
		filesystem: osfs.New(opts.CurrentDir),
	}

	return peer, nil
}

// Connections returns a list of connections.
func (p *Peer) Connections() []*Connection {
	return p.conns.list()
}

// FindConnection finds a connection by nickname. If no connection is found,
// then nil is returned.
func (p *Peer) FindConnection(nick string) *Connection {
	return p.conns.find(nick)
}

// Connect connects to the given address.
// If the connection is successful, then a new connection will be returned.
func (p *Peer) Connect(ctx context.Context, addr string, secret []byte) (*Connection, error) {
	p.logger.InfoContext(ctx,
		"connecting to peer",
		"addr", addr)

	netConn, err := p.Dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("could not dial address %s: %v", addr, err)
	}

	p.logger.DebugContext(ctx,
		"connected to peer, now initiating handshake",
		"real_addr", netConn.RemoteAddr(),
		"prev_addr", addr)

	peer := (*peerPrivate)(p)

	conn, err := newConnection(netConn, peer, p.logger)
	if err != nil {
		netConn.Close()
		return nil, fmt.Errorf("could not create connection: %v", err)
	}

	// Use the background context because the connection's lifetime should
	// outlive the Connect function.
	handleConn(context.Background(), p, &p.wg, conn, clientConnection)

	if err := conn.sendHello(ctx, secret); err != nil {
		conn.Close()
		return nil, fmt.Errorf("could not send hello message: %v", err)
	}

	return conn, nil
}

// BroadcastChat broadcasts a chat message to all connections.
func (p *Peer) BroadcastChat(ctx context.Context, content string) error {
	var wg sync.WaitGroup
	for _, conn := range p.Connections() {
		wg.Add(1)
		go func(conn *Connection) {
			defer wg.Done()
			if err := conn.SendChat(ctx, content); err != nil {
				p.logger.ErrorContext(ctx, "could not send chat message", "err", err)
			}
		}(conn)
	}
	wg.Wait()
	return ctx.Err()
}

// Close closes the peer and all of its connections.
// Note that this method does not affect the ListeningPeer returned by
// Listen. To close a ListeningPeer, call Close on the ListeningPeer.
func (p *Peer) Close() error {
	p.logger.Debug("closing peer")
	err := p.conns.close()
	p.wg.Wait()

	if p.recvClosed.CompareAndSwap(false, true) {
		p.Recv.Close()
	}

	return err
}

// ListeningPeer represents a peer that is listening for connections.
type ListeningPeer struct {
	*Peer

	// Addr is the address that the peer is listening on.
	Addr string

	wg       sync.WaitGroup
	listener net.Listener
}

// Listen starts listening for connections on the given address.
// It returns the address that the peer is listening on.
// If StartListening has already been called, then the function panics.
func (p *Peer) Listen(ctx context.Context, addr string) (*ListeningPeer, error) {
	listener, err := p.ListenConfig.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("could not listen on address %s: %v", addr, err)
	}

	// Spawn in our own context.
	//
	// This is intentional! The context passed in is only meant to be used for
	// the lifetime of the function call. We want to keep listening for
	// connections until the peer is closed (through p.Close()).

	l := &ListeningPeer{
		Peer:     p,
		Addr:     listener.Addr().String(),
		listener: listener,
	}

	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer listener.Close()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		for {
			netConn, err := listener.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return // intentionally cancelled
				}
				p.logger.Error(
					"could not accept connection",
					"error", err)
				return
			}

			conn, err := newConnection(netConn, (*peerPrivate)(p), p.logger)
			if err != nil {
				p.logger.Error(
					"could not create connection",
					"error", err)
				continue
			}

			handleConn(ctx, p, &l.wg, conn, serverConnection)
		}
	}()

	return l, nil
}

// Close closes the peer.
func (l *ListeningPeer) Close() error {
	l.logger.Debug("closing listening peer")
	err := l.listener.Close()
	l.wg.Wait()
	return err
}

func handleConn(
	ctx context.Context, p *Peer, wg *sync.WaitGroup,
	conn *Connection, role connectionRole,
) {
	ctx, cancel := context.WithCancel(ctx)

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()

		conn.handle(ctx, role)

		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			p.logger.Error(
				"could not close connection",
				"error", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()

		recvListener := conn.Recv.Listen()
		for {
			var msg message.Message
			var ok bool
			select {
			case <-ctx.Done():
				return
			case msg, ok = <-recvListener.C:
			}
			if !ok {
				break
			}

			connMsg := ConnectionMessage{
				Connection: conn,
				Message:    msg,
			}

			select {
			case <-ctx.Done():
				return
			case p.Recv.C <- connMsg:
			}
		}

		connMsg := ConnectionMessage{
			Connection: conn,
			Message:    &SystemConnectionTerminated{},
		}

		select {
		case <-ctx.Done():
		case p.Recv.C <- connMsg:
		}
	}()
}

type peerPrivate Peer

func (p *peerPrivate) Filesystem() billy.Filesystem {
	return p.filesystem
}

func (p *peerPrivate) Nickname() string {
	return p.Opts.Nickname
}

func (p *peerPrivate) CompareSecret(got []byte) bool {
	return bytes.Equal(p.Opts.Secret, got)
}

func (p *peerPrivate) ConnectionManager() *connectionManager {
	return p.conns
}
