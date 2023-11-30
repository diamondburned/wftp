package wftp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SierraSoftworks/multicast/v2"
	"github.com/go-git/go-billy/v5"
	"gopkg.in/typ.v4/sync2"
	"libdb.so/wftp/internal/atomic2"
	"libdb.so/wftp/message"
)

const defaultBufferSize = 4096 // 4 KB per chunk

type connectionPeer interface {
	// Filesystem returns the file system of the peer.
	Filesystem() billy.Filesystem
	// Nickname returns the Nickname of the user.
	Nickname() string
	// CompareSecret returns true if the secret matches the user's secret.
	CompareSecret(got []byte) bool
	// ConnectionManager returns the connection manager of the peer.
	// The connection will automatically add itself to the connection manager.
	ConnectionManager() *connectionManager
}

type connectionRole int

const (
	serverConnection connectionRole = iota
	clientConnection
)

type nicknameKind uint8

const (
	_ nicknameKind = iota
	serverAdvertisedNickname
	customUserNickname
)

type connectionState int32

const (
	stateConnecting connectionState = iota
	stateReady
	stateTerminating
	stateTerminated
)

func (s connectionState) String() string {
	switch s {
	case stateConnecting:
		return "connecting"
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

type receivingFile struct {
	file   io.WriteCloser
	buffer []byte
}

func newReceivingFile(file io.WriteCloser, bufferSize uint32) *receivingFile {
	return &receivingFile{
		file:   file,
		buffer: make([]byte, bufferSize),
	}
}

// Connection represents a peer-to-peer connection between two peers.
type Connection struct {
	// Recv is a channel of messages received from the peer.
	// Note that *Data messages are not sent on this channel.
	Recv *multicast.Channel[message.Message]
	// Send is a channel of messages to send to the peer.
	Send chan message.Message

	wg     sync.WaitGroup
	conn   net.Conn
	peer   connectionPeer
	logger *slog.Logger

	// getting keeps track of the files that we requested from the peer.
	// This prevents the peer from being able to upload files to us by sending a
	// malicious GetFileAgree message.
	//
	// It stores the destination directory as the value. If the value does not
	// contain a directory, then it will be created. If the value is empty, then
	// the file will be downloaded to the current directory.
	getting sync2.Map[message.FilePath, message.FilePath]

	// pending keeps track of the files that the peer wants to upload to us.
	pending sync2.Map[message.FilePath, message.PutFile]

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

func newConnection(netConn net.Conn, peer connectionPeer, logger *slog.Logger) (*Connection, error) {
	conn := &Connection{
		Recv: multicast.From(make(chan message.Message, 1)),
		Send: make(chan message.Message),

		conn:   netConn,
		peer:   peer,
		logger: logger,
	}

	conns := peer.ConnectionManager()
	if !conns.add(conn) {
		conn.Close()
		return nil, fmt.Errorf("connection already exists")
	}

	return conn, nil
}

// Close closes the connection.
func (c *Connection) Close() error {
	conns := c.peer.ConnectionManager()
	conns.remove(c)
	err := c.conn.Close()
	c.wg.Wait()
	return err
}

// Info returns information about this connection.
func (c *Connection) Info() ConnectionInfo {
	return c.peer.ConnectionManager().connInfo(c)
}

// SendChat sends a chat message to the peer.
func (c *Connection) SendChat(ctx context.Context, content string) error {
	return c.send(ctx, &message.Chat{Message: content})
}

// ListDirectory asks the peer to list the contents of the given directory.
func (c *Connection) ListDirectory(ctx context.Context, dir string) error {
	return c.send(ctx, &message.ListDirectory{Path: message.SanitizeFilePath(dir)})
}

// GetFile asks the peer to send us a file at the given path.
func (c *Connection) GetFile(ctx context.Context, path, dstDir string) error {
	// Normalize the paths to use forward slashes.
	msgPath := message.SanitizeFilePath(path)
	msgDstDir := message.SanitizeFilePath(dstDir)

	if _, exists := c.getting.LoadOrStore(msgPath, msgDstDir); exists {
		return fmt.Errorf("file already being downloaded")
	}

	return c.send(ctx, &message.GetFile{Path: msgPath})
}

// AskToPutFile asks the peer to put a file at the given path.
// In other words, this is a request to upload a file to the peer.
func (c *Connection) AskToPutFile(ctx context.Context, path, dstDir string) error {
	return c.send(ctx, &message.PutFile{
		Path:        message.SanitizeFilePath(path),
		Destination: message.SanitizeFilePath(dstDir),
	})
}

// AgreeToPutFile agrees to put a file at the given path.
// In other words, this is an agreement to upload a file to us.
// If dstDir is given, then the destination directory is also validated in case
// the peer swaps the destination directory with a malicious one last second.
func (c *Connection) AgreeToPutFile(ctx context.Context, path, dstDir string) error {
	msgPath := message.SanitizeFilePath(path)
	msgDstDir := message.SanitizeFilePath(dstDir)

	pending, ok := c.pending.Load(msgPath)
	if !ok {
		return fmt.Errorf("no pending put request for %q", path)
	}
	if dstDir != "" && pending.Destination != msgDstDir {
		return fmt.Errorf("destination directory mismatch, invalidating put request")
	}

	return c.send(ctx, &message.PutFileAgree{
		Path:        pending.Path,
		Destination: pending.Destination,
	})
}

// PendingPutRequests returns a list of paths that the peer wants to upload to
// us.
func (c *Connection) PendingPutRequests() []message.PutFile {
	var puts []message.PutFile
	c.pending.Range(func(_ message.FilePath, put message.PutFile) bool {
		puts = append(puts, put)
		return true
	})
	return puts
}

// SetNickname sets the nickname to the peer. It overrides the nickname that the
// user previously set, if any.
func (c *Connection) SetNickname(nick string) bool {
	return c.setNick(nick, customUserNickname)
}

func (c *Connection) setNick(nick string, kind nicknameKind) bool {
	conns := c.peer.ConnectionManager()
	return conns.setNick(c, nick, kind)
}

// addr returns the remote address of the connection.
func (c *Connection) addr() string {
	return c.conn.RemoteAddr().String()
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

func (c *Connection) sendHello(ctx context.Context, secret []byte) error {
	return c.send(ctx, &message.Hello{
		Nickname: c.peer.Nickname(),
		Secret:   secret,
	})
}

func (c *Connection) handle(ctx context.Context, role connectionRole) {
	ctx, cancel := context.WithCancel(ctx)
	l := connectionMainLoop{
		Connection: c,
		role:       role,
	}

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer c.logger.Debug("read loop exited")
		defer cancel()

		if err := l.handleRead(ctx); err != nil && ctx.Err() == nil {
			// Main loop's errors are all the other side's fault.
			// Send them a message and exit normally.
			c.sendError(ctx, err)
		}

		// Signal that no more messages will be received.
		c.Recv.Close()

		// Close all files that we're receiving.
		c.receivePool.Range(func(_ uint32, f *receivingFile) bool {
			f.file.Close()
			return true
		})
	}()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer c.logger.Debug("write loop exited")
		defer cancel()

		if err := l.handleWrite(ctx); err != nil && ctx.Err() == nil {
			c.logger.ErrorContext(ctx,
				"error occured trying to send messages, closing connection",
				"error", err)
		}
	}()

	c.wg.Wait()
}

type connectionMainLoop struct {
	*Connection
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
		case c.Recv.C <- msg:
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

			if !c.setNick(msg.Nickname, serverAdvertisedNickname) {
				return fmt.Errorf("nickname already taken")
			}

			if !c.state.CompareAndSwap(stateConnecting, stateReady) {
				return fmt.Errorf("already authenticated")
			}

			if err := c.send(ctx, &message.Welcome{
				Nickname: c.peer.Nickname(),
			}); err != nil {
				return fmt.Errorf("could not send hello message: %v", err)
			}

			c.logger.Info(
				"authenticated with peer",
				"peer", msg.Nickname,
				"addr", c.addr(),
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

			if !c.setNick(msg.Nickname, serverAdvertisedNickname) {
				return fmt.Errorf("nickname already taken")
			}

			// Server is saying hello to us. This means that we are
			// authenticated.
			if !c.state.CompareAndSwap(stateConnecting, stateReady) {
				return fmt.Errorf("already authenticated")
			}

			c.logger.Info(
				"authenticated with peer",
				"peer", msg.Nickname,
				"addr", c.addr(),
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
		case *message.PutFile:
			if err := c.receivedPutFile(ctx, msg); err != nil {
				return fmt.Errorf("could not handle put file: %v", err)
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
	entries, err := c.peer.Filesystem().ReadDir(string(msg.Path))
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
			Size:  uint64(entry.Size()),
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
	dstDir, requested := c.getting.LoadAndDelete(msg.Path)
	if !requested {
		return fmt.Errorf("unexpected get file agree message")
	}

	// Yes! We can now create this file on our end and start copying
	// data from the other side to it.

	fs := c.peer.Filesystem()
	dstPath := joinPathDestination(dstDir, msg.Path)

	if err := fs.MkdirAll(string(dstDir), os.ModePerm); err != nil {
		return fmt.Errorf("could not create directory: %v", err)
	}

	f, err := c.peer.Filesystem().Create(dstPath)
	if err != nil {
		c.sendError(ctx, fmt.Errorf("could not create file: %v", err))
		return nil
	}

	receiving := newReceivingFile(f, defaultBufferSize)

	_, exists := c.receivePool.LoadOrStore(msg.DataID, receiving)
	if exists {
		f.Close()
		return fmt.Errorf("duplicate data ID")
	}

	return nil
}

func (c *connectionMainLoop) receivedPutFile(ctx context.Context, msg *message.PutFile) error {
	_, exists := c.pending.LoadOrStore(msg.Path, *msg)
	if exists {
		c.sendError(ctx, fmt.Errorf("duplicate put file request"))
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
	w := bufio.NewWriterSize(c.conn, 1024)
	writeTimeout := 5 * time.Second

	writeMessage := func(msg message.Message) error {
		timeoutAt := time.Now().Add(writeTimeout)
		if err := c.conn.SetWriteDeadline(timeoutAt); err != nil {
			return fmt.Errorf("could not set write deadline: %v", err)
		}
		if err := message.Write(w, msg); err != nil {
			return fmt.Errorf("could not send message: %v", err)
		}
		if err := w.Flush(); err != nil {
			return fmt.Errorf("could not send message: %v", err)
		}
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			// Try to be graceful and deliver a terminate message before we
			// exit.
			if c.state.CompareAndSwap(stateReady, stateTerminated) {
				writeTimeout = 1 * time.Second
				writeMessage(&message.Terminate{})
			}

			return ctx.Err()

		case msg := <-c.Send:
			c.logger.Debug(
				"send",
				"type", msg.Type(),
				"state", c.state.Load())

			sendingMsg := msg

			switch msg := msg.(type) {
			case *message.Hello:
				// Forbid sending the wrong nickname.
				if msg.Nickname != c.peer.Nickname() {
					return fmt.Errorf("invalid nickname")
				}

			case *message.Terminate:
				// We want to terminate. Ensure we're in the right state.
				valid := false ||
					c.state.CompareAndSwap(stateReady, stateTerminating) ||
					c.state.CompareAndSwap(stateTerminating, stateTerminated)
				if !valid {
					return fmt.Errorf("invalid state for sending terminate message")
				}

			case *message.GetFile:
				if _, ok := c.getting.Load(msg.Path); !ok {
					return fmt.Errorf("unexpected get file message")
				}

			case *message.GetFileAgree:
				c.sendingGetFileAgree(ctx, msg)

			case *message.PutFileAgree:
				sendingMsg = c.sendingPutFileAgree(ctx, msg)
				if sendingMsg == nil {
					// Something went wrong. Don't send the message.
					continue
				}
			}

			if err := writeMessage(sendingMsg); err != nil {
				return err
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
	pending, ok := c.pending.LoadAndDelete(msg.Path)
	if !ok || pending.Path != msg.Path || pending.Destination != msg.Destination {
		// We're not expecting this file.
		c.logger.Warn(
			"unexpected put file agree message",
			"path", msg.Path,
			"destination", msg.Destination)
		return nil
	}

	fs := c.peer.Filesystem()
	dstPath := joinPathDestination(msg.Destination, msg.Path)

	if msg.Destination != "" && msg.Destination == "." {
		if err := fs.MkdirAll(string(msg.Destination), 0755); err != nil {
			c.logger.Error(
				"could not create directory for upload request",
				"error", err,
				"path", dstPath)
			return nil
		}
	}

	// We can now create this file on our end and start copying data
	// from the other side to it.
	f, err := c.peer.Filesystem().Create(dstPath)
	if err != nil {
		c.logger.Error(
			"could not create file for upload request",
			"error", err,
			"path", dstPath)
		return nil
	}

	// Allocate our own data ID for this file upload.
	dataID := c.lastFileSendID.Add(1)

	receiving := newReceivingFile(f, defaultBufferSize)
	c.receivePool.Store(dataID, receiving)

	// Shallow-copy the message and modify it to include
	// our data ID.
	clone := *msg
	clone.DataID = dataID
	clone.DataSize = defaultBufferSize

	return &clone
}

func (c *connectionMainLoop) transferFile(ctx context.Context, path message.FilePath, dataID uint32) {
	f, err := c.peer.Filesystem().Open(string(path))
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

func joinPathDestination(destination, name message.FilePath) string {
	return path.Join(string(destination), path.Base(string(name)))
}
