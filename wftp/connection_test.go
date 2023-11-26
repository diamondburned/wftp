package wftp

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"net"
	"path"
	"strings"
	"sync"
	"testing"

	"github.com/alecthomas/assert/v2"
	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/neilotoole/slogt"
	"libdb.so/cpsc-471-assignment/wftp/message"

	fsutil "github.com/go-git/go-billy/v5/util"
)

func TestConnection(t *testing.T) {
	tests := []struct {
		name string
		play func(t *testing.T, ctx context.Context, client *Connection, server *Connection)

		// secret sets the secret for the client and server. If nil, then
		// no secret is used.
		secret []byte

		initialServerFS map[string]string
		initialClientFS map[string]string

		finishedServerFS map[string]string
		finishedClientFS map[string]string
	}{
		{
			name: "handshake",
			play: func(t *testing.T, ctx context.Context, client, server *Connection) {
				assert.NoError(t, client.send(ctx, &message.Hello{
					Nickname: "client",
					Secret:   []byte("secret"),
				}))
				assertReceive(t, server, &message.Hello{
					Nickname: "client",
					Secret:   []byte("secret"),
				})
				assertReceive(t, client, &message.Welcome{
					Nickname: "server",
				})

				assert.NoError(t, client.send(ctx, &message.Terminate{}))
				assertReceive(t, server, &message.Terminate{})
				assertReceive(t, client, &message.Terminate{})
			},
			secret: []byte("secret"),
		},
		{
			name: "handshake server exits first",
			play: func(t *testing.T, ctx context.Context, client, server *Connection) {
				assert.NoError(t, client.send(ctx, &message.Hello{
					Nickname: "client",
					Secret:   []byte("secret"),
				}))
				assertReceive(t, server, &message.Hello{
					Nickname: "client",
					Secret:   []byte("secret"),
				})
				assertReceive(t, client, &message.Welcome{
					Nickname: "server",
				})

				assert.NoError(t, server.send(ctx, &message.Terminate{}))
				assertReceive(t, client, &message.Terminate{})
				assertReceive(t, server, &message.Terminate{})
			},
			secret: []byte("secret"),
		},
		{
			name: "list files",
			play: func(t *testing.T, ctx context.Context, client, server *Connection) {
				assert.NoError(t, client.send(ctx, &message.Hello{Nickname: "client"}))
				assertReceive(t, server, &message.Hello{Nickname: "client"})
				assertReceive(t, client, &message.Welcome{Nickname: "server"})

				assert.NoError(t, client.send(ctx, &message.ListDirectory{Path: "/"}))
				assertReceive(t, server, &message.ListDirectory{Path: "/"})
				assertReceive(t, client, &message.DirectoryList{
					Path: "/",
					Entries: []message.DirectoryEntry{
						{Name: "bar.txt", Mode: 0666},
						{Name: "dir", Mode: fs.ModeDir | 0755, IsDir: true},
						{Name: "foo.exe", Mode: 0666},
					},
				})

				assert.NoError(t, client.send(ctx, &message.Terminate{}))
				assertReceive(t, server, &message.Terminate{})
				assertReceive(t, client, &message.Terminate{})
			},
			initialServerFS: map[string]string{
				"foo.exe":     "foo",
				"bar.txt":     "bar",
				"dir/baz.txt": "baz",
			},
		},
		// Note to future self: we're never testing for FileTransferData, since
		// we're never emitting it for performance reasons.
		{
			name: "file download",
			play: func(t *testing.T, ctx context.Context, client, server *Connection) {
				assert.NoError(t, client.send(ctx, &message.Hello{Nickname: "client"}))
				assertReceive(t, server, &message.Hello{Nickname: "client"})
				assertReceive(t, client, &message.Welcome{Nickname: "server"})

				assert.NoError(t, client.send(ctx, &message.GetFile{Path: "foo.exe"}))
				assertReceive(t, server, &message.GetFile{Path: "foo.exe"})
				assertReceive(t, client, &message.GetFileAgree{
					Path:     "foo.exe",
					DataID:   1,
					DataSize: defaultBufferSize,
				})
				assertReceive(t, client, &message.FileTransferEnd{
					DataID: 1,
				})

				assert.NoError(t, client.send(ctx, &message.Terminate{}))
				assertReceive(t, server, &message.Terminate{})
				assertReceive(t, client, &message.Terminate{})
			},
			initialServerFS: map[string]string{
				"foo.exe":     "foo",
				"bar.txt":     "bar",
				"dir/baz.txt": "baz",
			},
			initialClientFS: map[string]string{
				"bar.txt": "bar",
			},
			finishedClientFS: map[string]string{
				"foo.exe": "foo",
				"bar.txt": "bar",
			},
		},
		{
			name: "file upload",
			play: func(t *testing.T, ctx context.Context, client, server *Connection) {
				assert.NoError(t, client.send(ctx, &message.Hello{Nickname: "client"}))
				assertReceive(t, server, &message.Hello{Nickname: "client"})
				assertReceive(t, client, &message.Welcome{Nickname: "server"})

				assert.NoError(t, client.send(ctx, &message.PutFile{Path: "baz.txt"}))
				assertReceive(t, server, &message.PutFile{Path: "baz.txt"})

				assert.NoError(t, server.agreeToPutFile(ctx, &message.PutFile{Path: "baz.txt"}))
				assertReceive(t, client, &message.PutFileAgree{
					Path:     "baz.txt",
					DataID:   1,
					DataSize: defaultBufferSize,
				})
				assertReceive(t, server, &message.FileTransferEnd{
					DataID: 1,
				})

				assert.NoError(t, client.send(ctx, &message.Terminate{}))
				assertReceive(t, server, &message.Terminate{})
				assertReceive(t, client, &message.Terminate{})
			},
			initialServerFS: map[string]string{
				"foo.exe":     "foo",
				"bar.txt":     "bar",
				"dir/baz.txt": "baz",
			},
			initialClientFS: map[string]string{
				"baz.txt": "baz",
			},
			finishedServerFS: map[string]string{
				"foo.exe":     "foo",
				"bar.txt":     "bar",
				"baz.txt":     "baz",
				"dir/baz.txt": "baz",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			serverFS := createTestFS(t, test.initialServerFS)
			clientFS := createTestFS(t, test.initialClientFS)

			ctx, server, client := startServerClientPair(t, testOpts{
				secret:   test.secret,
				serverFS: serverFS,
				clientFS: clientFS,
			})

			test.play(t, ctx, client, server)
			t.Log("play done")

			assertChannelClosed(t, server.Recv)
			assertChannelClosed(t, client.Recv)

			if test.finishedServerFS != nil {
				gotServerFS := extractTestFS(t, serverFS)
				assert.Equal(t, test.finishedServerFS, gotServerFS, "server filesystem mismatch")
			}

			if test.finishedClientFS != nil {
				gotClientFS := extractTestFS(t, clientFS)
				assert.Equal(t, test.finishedClientFS, gotClientFS, "client filesystem mismatch")
			}
		})
	}
}

func assertReaderClosed(t *testing.T, r io.Reader) {
	t.Helper()
	_, err := r.Read(make([]byte, 1))
	assert.Error(t, err)
}

func assertWriterClosed(t *testing.T, w io.Writer) {
	t.Helper()
	_, err := w.Write([]byte{})
	assert.Error(t, err)
}

func assertChannelClosed[T any](t *testing.T, ch <-chan T) {
	t.Helper()
	v, ok := <-ch
	if ok {
		t.Errorf("channel not closed, received %#v", v)
	}
}

type testOpts struct {
	secret   []byte
	clientFS billy.Filesystem
	serverFS billy.Filesystem
}

func startServerClientPair(t *testing.T, opts testOpts) (ctx context.Context, server, client *Connection) {
	logger := slogt.New(t)

	serverConn, clientConn := net.Pipe()

	serverPeer := &connectionPeerStub{
		nickname:   "server",
		filesystem: opts.serverFS,
		secret:     opts.secret,
		connMan:    newConnectionManager(),
	}

	clientPeer := &connectionPeerStub{
		nickname:   "client",
		filesystem: opts.clientFS,
		connMan:    newConnectionManager(),
	}

	ctx, cancel := context.WithCancel(context.Background())

	server = newConnection(serverConn, serverPeer, logger)
	client = newConnection(clientConn, clientPeer, logger)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { server.handle(ctx, serverConnection); wg.Done() }()
	go func() { client.handle(ctx, clientConnection); wg.Done() }()

	t.Cleanup(func() {
		cancel()
		t.Log("waiting for connections to finish")
		wg.Wait()
		serverConn.Close()
		clientConn.Close()
	})

	return
}

func createTestFS(t *testing.T, files map[string]string) billy.Filesystem {
	t.Helper()

	fs := memfs.New()

	for name, data := range files {
		err := fs.MkdirAll(path.Dir(name), 0755)
		assert.NoError(t, err)

		f, err := fs.Create(path.Clean(name))
		assert.NoError(t, err)

		_, err = f.Write([]byte(data))
		assert.NoError(t, err)

		err = f.Close()
		assert.NoError(t, err)
	}

	return fs
}

func extractTestFS(t *testing.T, billyFS billy.Filesystem) map[string]string {
	t.Helper()

	files := map[string]string{}
	err := fsutil.Walk(billyFS, "/", func(name string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		b, err := fsutil.ReadFile(billyFS, name)
		assert.NoError(t, err)

		name = path.Clean(name)
		name = strings.TrimPrefix(name, "/")

		files[name] = string(b)
		return nil
	})
	assert.NoError(t, err)

	return files
}

func assertReceive(t *testing.T, conn *Connection, msg message.Message) {
	t.Helper()
	got, ok := <-conn.Recv
	if !ok {
		t.Errorf("receive channel closed unexpectedly while waiting for %T", msg)
		return
	}

	if errorMsg, ok := got.(*message.Error); ok {
		t.Error("received error:", errorMsg.Message)
		return
	}

	assert.Equal(t, msg, got, conn.peer.Nickname()+" received unexpected message:")
}

type testLogger struct {
	t *testing.T
}

type connectionPeerStub struct {
	filesystem billy.Filesystem
	nickname   string
	secret     []byte
	connMan    *ConnectionManager
}

func (p *connectionPeerStub) Filesystem() billy.Filesystem {
	return p.filesystem
}

func (p *connectionPeerStub) Nickname() string {
	return p.nickname
}

func (p *connectionPeerStub) AddNick(conn *Connection, nick string) bool {
	return p.connMan.addNick(conn, nick)
}

func (p *connectionPeerStub) CompareSecret(got []byte) bool {
	return bytes.Equal(p.secret, got)
}
