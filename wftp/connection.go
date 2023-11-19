package wftp

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"

	"libdb.so/cpsc-471-assignment/wftp/message"
)

// Connection represents a peer-to-peer connection between two peers.
type Connection struct {
	// peer is the peer this connection belongs to.
	peer *Peer

	conn io.ReadWriteCloser
	ids  []string

	wg     sync.WaitGroup
	ftp    *fileTransferPool
	logger *slog.Logger
}

func newConnection(peer *Peer, conn net.Conn) *Connection {
	return &Connection{
		peer: peer,
		conn: conn,
		ids:  []string{conn.RemoteAddr().String()},
		ftp:  newFileTransferPool(),
		logger: peer.logger.With(
			"addr", conn.RemoteAddr()),
	}
}

func (c *Connection) send(msg message.Message) error {
	if err := msg.Encode(c.conn); err != nil {
		return fmt.Errorf("could not send message: %v", err)
	}
	return nil
}

func (c *Connection) sendError(err error) {
	c.logger.Warn(
		"error occurred",
		"error", err)

	if err := c.send(&message.Error{Message: err.Error()}); err != nil {
		c.logger.Error(
			"could not send error message",
			"error", err)
	}
}

func (c *Connection) handleConn(ctx context.Context) error {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		<-ctx.Done()
		c.conn.Close()
	}()

	buf := bufio.NewReaderSize(c.conn, 1024)
	return c.handleReader(ctx, buf)
}

func (c *Connection) handleReader(ctx context.Context, r io.Reader) error {
	var authenticated bool

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	defer c.ftp.closeAll()

	for {
		t, err := message.ReadType(r)
		if err != nil {
			return fmt.Errorf("could not read message: %v", err)
		}

		switch t {
		case message.MessageTypeHello:
			var msg message.Hello
			if err := msg.Decode(r); err != nil {
				return fmt.Errorf("could not read hello message: %v", err)
			}

			// Client is saying hello to us.
			if authenticated {
				c.sendError(fmt.Errorf("already authenticated"))
				continue
			}

			if !bytes.Equal(msg.Secret, c.peer.secret) {
				c.sendError(fmt.Errorf("invalid secret"))
				continue
			}

			if !c.peer.connMan.addNick(c, msg.Nickname) {
				c.sendError(fmt.Errorf("nickname already taken"))
				continue
			}

			authenticated = true
			if err := c.send(&message.Hello{
				Nickname: c.peer.nickname,
			}); err != nil {
				return fmt.Errorf("could not send hello message: %v", err)
			}

		case message.MessageTypeError:
			var msg message.Error
			if err := msg.Decode(r); err != nil {
				return fmt.Errorf("could not read error message: %v", err)
			}

			// Client is saying that an error occurred.
			c.logger.Error(
				"client error",
				"error", msg.Message)

		case message.MessageTypeTerminate:
			var msg message.Terminate
			if err := msg.Decode(r); err != nil {
				return fmt.Errorf("could not read terminate message: %v", err)
			}

			// Client is saying that it is terminating.
			c.logger.Info(
				"client terminating")

			if err := c.send(&message.Terminate{}); err != nil {
				return fmt.Errorf("could not send terminate message: %v", err)
			}

		case message.MessageTypeTerminated:
			var msg message.Terminated
			if err := msg.Decode(r); err != nil {
				return fmt.Errorf("could not read terminated message: %v", err)
			}

			// Client is saying that it has terminated.
			c.logger.Info(
				"client terminated",
				"had_outstanding_requests", msg.HadOutstandingRequests)

			return nil
		}

		// Anything beyond this point requires authentication.
		if !authenticated {
			c.sendError(fmt.Errorf("not authenticated"))
			continue
		}

		switch t {
		case message.MessageTypeListDirectory:
			var msg message.ListDirectory
			if err := msg.Decode(r); err != nil {
				return fmt.Errorf("could not read list directory message: %v", err)
			}

			if err := c.handleListDirectory(ctx, &msg); err != nil {
				return fmt.Errorf("could not handle list directory: %v", err)
			}

		case message.MessageTypeGetFile:
			var msg message.GetFile
			if err := msg.Decode(r); err != nil {
				return fmt.Errorf("could not read get file message: %v", err)
			}

			if err := c.handleGetFile(ctx, msg); err != nil {
				return fmt.Errorf("could not handle get file: %v", err)
			}

		case message.MessageTypeFileUpload:
			var msg message.FileUpload
			if err := msg.Decode(r); err != nil {
				return fmt.Errorf("could not read file upload message: %v", err)
			}

			if err := c.handleFileUpload(ctx, msg); err != nil {
				return fmt.Errorf("could not handle file upload: %v", err)
			}

		case message.MessageTypeFileUploadData:
			var msg message.FileUploadData
			if err := msg.DecodeDataID(r); err != nil {
				return fmt.Errorf("could not read file upload data message: %v", err)
			}

			fw := c.ftp.getWriter(msg.DataID)
			if fw == nil {
				return fmt.Errorf("could not find file upload data writer")
			}

			msg.Data = fw.buffer
			if err := msg.DecodeData(r); err != nil {
				return fmt.Errorf("could not read file upload data message: %v", err)
			}

			if err := c.handleFileUploadData(ctx, msg, fw); err != nil {
				return fmt.Errorf("could not handle file upload data: %v", err)
			}

		default:
			return fmt.Errorf("unknown message type: %v", t)
		}
	}
}

func (c *Connection) handleListDirectory(ctx context.Context, msg *message.ListDirectory) error {
	entries, err := os.ReadDir(msg.Path)
	if err != nil {
		return fmt.Errorf("could not read directory: %v", err)
	}

	resp := message.DirectoryList{
		Entries: make([]message.DirectoryEntry, len(entries)),
	}
	for i, entry := range entries {
		resp.Entries[i] = message.DirectoryEntry{
			Name:  entry.Name(),
			Mode:  entry.Type(),
			IsDir: entry.IsDir(),
		}
	}

	return c.send(&resp)
}

const defaultBufferSize = 4096 // 4 KB per chunk

func (c *Connection) handleGetFile(ctx context.Context, msg message.GetFile) error {
	f, err := os.Open(msg.Path)
	if err != nil {
		return fmt.Errorf("could not open file: %v", err)
	}

	size, err := getFilesize(f)
	if err != nil {
		f.Close()
		return fmt.Errorf("could not get file size: %v", err)
	}

	ftID := c.ftp.add()

	resp := message.FileTransferBegin{
		Path:     msg.Path,
		Size:     uint64(size),
		DataID:   ftID,
		DataSize: defaultBufferSize,
	}
	if err := c.send(&resp); err != nil {
		f.Close()
		return err
	}

	go func() {
		if err := c.startFileTransfer(ctx, f, ftID); err != nil {
			c.sendError(fmt.Errorf("could not transfer file: %v", err))
		}
	}()

	return nil
}

func (c *Connection) startFileTransfer(ctx context.Context, src io.ReadCloser, ftID uint32) error {
	defer src.Close()

	buf := make([]byte, defaultBufferSize)
	for {
		n, err := src.Read(buf)
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("could not read file: %v", err)
		}

		buf := buf[:n]
		msg := message.FileTransferData{
			DataID: ftID,
			Data:   buf,
		}

		if err := c.send(&msg); err != nil {
			return fmt.Errorf("could not send file data: %v", err)
		}
	}

	return nil
}

func (c *Connection) handleFileUpload(ctx context.Context, msg message.FileUpload) error {
	f, err := os.Create(msg.Path)
	if err != nil {
		c.sendError(fmt.Errorf("could not create file: %v", err))
		return nil
	}

	size, err := getFilesize(f)
	if err != nil {
		f.Close()
		return fmt.Errorf("could not get file size: %v", err)
	}

	fw := c.ftp.addWriter(f, uint64(size), defaultBufferSize)

	resp := message.FileTransferBegin{
		Path:     msg.Path,
		Size:     msg.Size,
		DataID:   fw.id,
		DataSize: defaultBufferSize,
	}
	if err := c.send(&resp); err != nil {
		f.Close()
		return err
	}

	return nil
}

func (c *Connection) handleFileUploadData(ctx context.Context, msg message.FileUploadData, fw *fileWriter) error {
	_, err := fw.Write(msg.Data)
	if err != nil {
		c.ftp.remove(fw.id)
		fw.dst.Close()
		c.sendError(err)
		return nil
	}

	if fw.isDone() {
		c.ftp.remove(fw.id)
		fw.dst.Close()
	}

	return nil
}

func getFilesize(f *os.File) (int64, error) {
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, fmt.Errorf("could not seek to end of file: %v", err)
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("could not seek to start of file: %v", err)
	}

	return size, nil
}
