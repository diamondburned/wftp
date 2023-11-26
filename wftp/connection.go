package wftp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-git/go-billy/v5"
	"gopkg.in/typ.v4/sync2"
	"libdb.so/cpsc-471-assignment/wftp/internal/atomic2"
	"libdb.so/cpsc-471-assignment/wftp/message"
)

const defaultBufferSize = 4096 // 4 KB per chunk

type connectionPeer interface {
	// Filesystem returns the file system of the peer.
	Filesystem() billy.Filesystem
	// Nickname returns the Nickname of the user.
	Nickname() string
	// AddNick adds a nickname to the user.
	AddNick(*Connection, string) bool
	// CompareSecret returns true if the secret matches the user's secret.
	CompareSecret(got []byte) bool
}

type connectionRole int

const (
	serverConnection connectionRole = iota
	clientConnection
)

type nicknameKind uint8

const (
	remoteAddrNickname nicknameKind = iota
	customUserNickname
	serverAdvertisedNickname
)

type connectionState int32

const (
	stateInitial connectionState = iota
	stateReady
	stateTerminating
	stateTerminated
)

func (s connectionState) String() string {
	switch s {
	case stateInitial:
		return "initial"
	case stateReady:
		return "ready"
	case stateTerminating:
		return "terminating"
	case stateTerminated:
		return "terminated"
	default:
		return fmt.Sprintf("connectionState(%d)", s)
	}
}

// Connection represents a peer-to-peer connection between two peers.
type Connection struct {
	// Recv is a channel of messages received from the peer.
	// Note that *Data messages are not sent on this channel.
	Recv chan message.Message
	// Send is a channel of messages to send to the peer.
	Send chan message.Message

	conn   net.Conn
	peer   connectionPeer
	nicks  map[string]nicknameKind // destination's nicknames
	logger *slog.Logger

	// getting keeps track of the files that we requested from the peer.
	// This prevents the peer from being able to upload files to us by sending a
	// malicious GetFileAgree message.
	getting sync2.Map[string, struct{}]

	// receivePool tracks all files that we're receiving from the peer.
	// It uses data ID that the peer sends us as the key.
	receivePool sync2.Map[uint32, *receivingFile]

	// lastFileSendID is the last file ID that we sent to the peer.
	// We use this to generate unique file IDs.
	lastFileSendID atomic.Uint32

	// state is the current state of the connection.
	// Refer to the connectionState constants for possible values.
	state atomic2.Int32[connectionState]
}

func newConnection(conn net.Conn, peer connectionPeer, logger *slog.Logger) *Connection {
	logger = logger.With(
		"peer", peer.Nickname(),
		"local_addr", conn.LocalAddr(),
		"remote_addr", conn.RemoteAddr(),
	)

	nicks := make(map[string]nicknameKind)
	nicks[conn.RemoteAddr().String()] = remoteAddrNickname

	return &Connection{
		Recv: make(chan message.Message, 1),
		Send: make(chan message.Message),

		conn:   conn,
		peer:   peer,
		nicks:  nicks,
		logger: logger,
	}
}

func (c *Connection) Close() error {
	return c.conn.Close()
}

func (c *Connection) send(ctx context.Context, msg message.Message) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case c.Send <- msg:
		return nil
	}
}

func (c *Connection) sendError(ctx context.Context, err error) error {
	return c.send(ctx, &message.Error{Message: err.Error()})
}

func (c *Connection) askToPutFile(ctx context.Context, path string) error {
	return c.send(ctx, &message.PutFile{Path: path})
}

func (c *Connection) agreeToPutFile(ctx context.Context, putFile *message.PutFile) error {
	return c.send(ctx, &message.PutFileAgree{Path: putFile.Path})
}

func (c *Connection) connectionInfo() ConnectionInfo {
	var info ConnectionInfo
	for nick, role := range c.nicks {
		switch role {
		case remoteAddrNickname:
			info.RemoteAddr = nick
		case serverAdvertisedNickname:
			info.ServerName = nick
		case customUserNickname:
			info.Nicknames = append(info.Nicknames, nick)
		}
	}
	return info
}

func (c *Connection) addNick(nick string, kind nicknameKind) bool {
	if !c.peer.AddNick(c, nick) {
		return false
	}
	c.nicks[nick] = kind
	return true
}

func (c *Connection) handle(ctx context.Context, role connectionRole) {
	var wg sync.WaitGroup
	defer wg.Wait()

	ctx, cancel := context.WithCancel(ctx)
	l := connectionMainLoop{
		Connection: c,
		wg:         &wg,
		role:       role,
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer c.logger.Debug("read loop exited")
		defer cancel()

		if err := l.handleRead(ctx); err != nil {
			// Main loop's errors are all the other side's fault.
			// Send them a message and exit normally.
			c.sendError(ctx, err)
		}

		// Signal that no more messages will be received.
		close(c.Recv)

		// Close all files that we're receiving.
		c.receivePool.Range(func(_ uint32, f *receivingFile) bool {
			f.file.Close()
			return true
		})
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer c.logger.Debug("write loop exited")
		defer cancel()

		if err := l.handleWrite(ctx); err != nil {
			c.logger.ErrorContext(ctx,
				"error occured trying to send messages, closing connection",
				"error", err)
		}
	}()
}

type connectionMainLoop struct {
	*Connection
	wg   *sync.WaitGroup
	role connectionRole
}

func (c *connectionMainLoop) isRole(role connectionRole) bool {
	return c.role == role
}

func (c *connectionMainLoop) handleRead(ctx context.Context) error {
	r := bufio.NewReaderSize(c.conn, 1024)

	emit := func(msg message.Message) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case c.Recv <- msg:
			return nil
		}
	}

	decode := func(msg message.Message) error {
		if err := msg.Decode(r); err != nil {
			return fmt.Errorf("could not read message: %v", err)
		}
		return nil
	}

	for {
		t, err := message.ReadType(r)
		if err != nil {
			return fmt.Errorf("could not read message: %v", err)
		}

		c.logger.Debug(
			"recv",
			"type", t)

		switch t {
		case message.MessageTypeHello:
			if !c.isRole(serverConnection) {
				return fmt.Errorf("unexpected hello message")
			}

			var msg message.Hello
			if err := decode(&msg); err != nil {
				return fmt.Errorf("could not read hello message: %v", err)
			}

			if !c.peer.CompareSecret(msg.Secret) {
				return fmt.Errorf("invalid secret")
			}

			if !c.addNick(msg.Nickname, serverAdvertisedNickname) {
				return fmt.Errorf("nickname already taken")
			}

			if !c.state.CompareAndSwap(stateInitial, stateReady) {
				return fmt.Errorf("already authenticated")
			}

			if err := c.send(ctx, &message.Welcome{
				Nickname: c.peer.Nickname(),
			}); err != nil {
				return fmt.Errorf("could not send hello message: %v", err)
			}

			c.logger.Debug(
				"authenticated with client",
				"nickname", msg.Nickname,
				"state", c.state.Load())

			emit(&msg)
			continue

		case message.MessageTypeWelcome:
			if !c.isRole(clientConnection) {
				return fmt.Errorf("unexpected welcome message")
			}

			var msg message.Welcome
			if err := decode(&msg); err != nil {
				return fmt.Errorf("could not read welcome message: %v", err)
			}

			if !c.addNick(msg.Nickname, serverAdvertisedNickname) {
				return fmt.Errorf("nickname already taken")
			}

			// Server is saying hello to us. This means that we are
			// authenticated.
			if !c.state.CompareAndSwap(stateInitial, stateReady) {
				return fmt.Errorf("already authenticated")
			}

			c.logger.Debug(
				"authenticated with server",
				"nickname", msg.Nickname,
				"state", c.state.Load())

			emit(&msg)
			continue

		case message.MessageTypeError:
			var msg message.Error
			if err := decode(&msg); err != nil {
				return fmt.Errorf("could not read error message: %v", err)
			}

			c.logger.Error(
				"received error from other side",
				"error", msg.Message,
				"state", c.state.Load())

			emit(&msg)
			continue

		case message.MessageTypeTerminate:
			var msg message.Terminate
			if err := decode(&msg); err != nil {
				return fmt.Errorf("could not read terminate message: %v", err)
			}

			if c.state.CompareAndSwap(stateReady, stateTerminating) {
				// Other side is requesting that we terminate.
				c.logger.Info(
					"peer terminating",
					"state", c.state.Load())

				// Respond back with the same message, acknowledging that we are
				// terminating.
				if err := c.send(ctx, &message.Terminate{}); err != nil {
					return fmt.Errorf("could not send terminate message: %v", err)
				}

				emit(&msg)
				return nil
			}

			if c.state.CompareAndSwap(stateTerminating, stateTerminated) {
				// We requested that the other side terminate, and they are
				// acknowledging that they are terminating.
				c.logger.Info(
					"peer terminated",
					"state", c.state.Load())

				emit(&msg)
				return nil
			}

			return fmt.Errorf("unexpected terminate message")
		}

		// Anything beyond this point requires authentication.
		if c.state.Load() != stateReady {
			return fmt.Errorf("not authenticated/ready")
		}

		if t == message.MessageTypeFileTransferData {
			// This message specifically needs to be handled efficiently, so we
			// hand it the entire reader.
			if err := c.receivedFileTransferData(ctx, r); err != nil {
				return fmt.Errorf("could not handle file transfer data: %v", err)
			}
			// Don't emit this message, as it is handled specially.
			continue
		}

		msg, err := message.Decode(r, t)
		if err != nil {
			return fmt.Errorf("could not decode message: %v", err)
		}

		switch msg := msg.(type) {
		case *message.ListDirectory:
			if err := c.receivedListDirectory(ctx, msg); err != nil {
				return fmt.Errorf("could not handle list directory: %v", err)
			}
		case *message.GetFile:
			if err := c.receivedGetFile(ctx, msg); err != nil {
				return fmt.Errorf("could not handle get file: %v", err)
			}
		case *message.GetFileAgree:
			if err := c.receivedGetFileAgree(ctx, msg); err != nil {
				return fmt.Errorf("could not handle get file agree: %v", err)
			}
		case *message.PutFileAgree:
			if err := c.receivedPutFileAgree(ctx, msg); err != nil {
				return fmt.Errorf("could not handle put file agree: %v", err)
			}
		case *message.FileTransferEnd:
			if err := c.receivedFileTransferEnd(ctx, msg); err != nil {
				return fmt.Errorf("could not handle file transfer end: %v", err)
			}
		}

		emit(msg)
	}
}

func (c *connectionMainLoop) receivedListDirectory(ctx context.Context, msg *message.ListDirectory) error {
	entries, err := c.peer.Filesystem().ReadDir(msg.Path)
	if err != nil {
		return fmt.Errorf("could not read directory: %v", err)
	}

	resp := message.DirectoryList{
		Path:    msg.Path,
		Entries: make([]message.DirectoryEntry, len(entries)),
	}
	for i, entry := range entries {
		resp.Entries[i] = message.DirectoryEntry{
			Name:  entry.Name(),
			Mode:  entry.Mode(),
			IsDir: entry.IsDir(),
		}
	}

	return c.send(ctx, &resp)
}

func (c *connectionMainLoop) receivedGetFile(ctx context.Context, msg *message.GetFile) error {
	dataID := c.lastFileSendID.Add(1)

	return c.send(ctx, &message.GetFileAgree{
		Path:     msg.Path,
		DataID:   dataID,
		DataSize: defaultBufferSize,
	})
}

func (c *connectionMainLoop) receivedGetFileAgree(ctx context.Context, msg *message.GetFileAgree) error {
	// Did we request this file?
	if _, requested := c.getting.LoadAndDelete(msg.Path); !requested {
		return fmt.Errorf("unexpected get file agree message")
	}

	// Yes! We can now create this file on our end and start copying
	// data from the other side to it.
	f, err := c.peer.Filesystem().Create(msg.Path)
	if err != nil {
		c.sendError(ctx, fmt.Errorf("could not create file: %v", err))
		return nil
	}

	receiving := newReceivingFile(f, msg.Path, defaultBufferSize)

	_, exists := c.receivePool.LoadOrStore(msg.DataID, receiving)
	if exists {
		f.Close()
		return fmt.Errorf("duplicate data ID")
	}

	return nil
}

func (c *connectionMainLoop) receivedPutFileAgree(ctx context.Context, msg *message.PutFileAgree) error {
	c.wg.Add(1)
	go func() {
		c.transferFile(ctx, msg.Path, msg.DataID)
		c.wg.Done()
	}()

	return nil
}

func (c *connectionMainLoop) receivedFileTransferData(ctx context.Context, r io.Reader) error {
	var msg message.FileTransferData
	if err := msg.DecodeDataID(r); err != nil {
		return fmt.Errorf("could not decode file transfer data: %v", err)
	}

	receiving, ok := c.receivePool.Load(msg.DataID)
	if !ok {
		return fmt.Errorf("unexpected data ID")
	}

	msg.Data = receiving.buffer
	if err := msg.DecodeData(r); err != nil {
		return fmt.Errorf("could not decode file transfer data: %v", err)
	}

	_, err := receiving.file.Write(msg.Data)
	if err != nil {
		c.sendError(ctx, fmt.Errorf("could not write file: %v", err))
		receiving.file.Close()
		return nil
	}

	return nil
}

func (c *connectionMainLoop) receivedFileTransferEnd(ctx context.Context, msg *message.FileTransferEnd) error {
	receiving, ok := c.receivePool.LoadAndDelete(msg.DataID)
	if !ok {
		return fmt.Errorf("unexpected data ID")
	}

	if err := receiving.file.Close(); err != nil {
		c.logger.Warn(
			"could not close received file after graceful transfer",
			"error", err)
	}

	return nil
}

func (c *connectionMainLoop) handleWrite(ctx context.Context) error {
	const writeTimeout = 5 * time.Second

	w := bufio.NewWriterSize(c.conn, 1024)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case msg := <-c.Send:
			timeoutAt := time.Now().Add(writeTimeout)
			c.logger.Debug(
				"send",
				"type", msg.Type(),
				"timeout_at", timeoutAt,
				"state", c.state.Load())

			sendingMsg := msg

			switch msg := msg.(type) {
			case *message.Hello:
				// Forbid sending the wrong nickname.
				msg.Nickname = c.peer.Nickname()

			case *message.Terminate:
				// We want to terminate. Ensure we're in the right state.
				valid := false ||
					c.state.CompareAndSwap(stateReady, stateTerminating) ||
					c.state.CompareAndSwap(stateTerminating, stateTerminated)
				if !valid {
					return fmt.Errorf("invalid state for sending terminate message")
				}

			case *message.GetFile:
				// Mark that we consent to receiving this file.
				c.getting.Store(msg.Path, struct{}{})

			case *message.GetFileAgree:
				c.sendingGetFileAgree(ctx, msg)

			case *message.PutFileAgree:
				sendingMsg = c.sendingPutFileAgree(ctx, msg)
				if sendingMsg == nil {
					// Something went wrong. Don't send the message.
					continue
				}
			}

			if err := c.conn.SetWriteDeadline(timeoutAt); err != nil {
				return fmt.Errorf("could not set write deadline: %v", err)
			}

			if err := message.Write(w, sendingMsg); err != nil {
				return fmt.Errorf("could not send message: %v", err)
			}

			if err := w.Flush(); err != nil {
				return fmt.Errorf("could not send message: %v", err)
			}
		}
	}
}

func (c *connectionMainLoop) sendingGetFileAgree(ctx context.Context, msg *message.GetFileAgree) {
	// We've just agreed to sending a file to the peer.
	// Begin this process.
	c.wg.Add(1)
	go func() {
		c.transferFile(ctx, msg.Path, msg.DataID)
		c.wg.Done()
	}()
}

func (c *connectionMainLoop) sendingPutFileAgree(ctx context.Context, msg *message.PutFileAgree) message.Message {
	// We can now create this file on our end and start copying data
	// from the other side to it.
	f, err := c.peer.Filesystem().Create(msg.Path)
	if err != nil {
		c.logger.Error(
			"could not create file for upload request",
			"error", err,
			"path", msg.Path)
		return nil
	}

	// Allocate our own data ID for this file upload.
	dataID := c.lastFileSendID.Add(1)

	receiving := newReceivingFile(f, msg.Path, defaultBufferSize)
	c.receivePool.Store(dataID, receiving)

	// Shallow-copy the message and modify it to include
	// our data ID.
	clone := *msg
	clone.DataID = dataID
	clone.DataSize = defaultBufferSize

	return &clone
}

func (c *connectionMainLoop) transferFile(ctx context.Context, path string, dataID uint32) {
	f, err := c.peer.Filesystem().Open(path)
	if err != nil {
		c.sendError(ctx, fmt.Errorf("could not open file: %v", err))
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()

		<-ctx.Done()
		f.Close()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()

		defer cancel()
		defer f.Close()

		buf := make([]byte, defaultBufferSize)
		for {
			n, err := f.Read(buf)
			if err != nil {
				if err == io.EOF {
					break
				}
				c.sendError(ctx, fmt.Errorf("could not read file: %v", err))
				return
			}

			if err := c.send(ctx, &message.FileTransferData{
				DataID: dataID,
				Data:   buf[:n],
			}); err != nil {
				return
			}
		}

		c.send(ctx, &message.FileTransferEnd{
			DataID: dataID,
		})
	}()

	wg.Wait()
}
