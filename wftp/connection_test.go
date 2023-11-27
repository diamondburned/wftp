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
		play func(t *testing.T, ctx context.Context, client, server *testConnection)

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
			play: func(t *testing.T, ctx context.Context, client, server *testConnection) {
				assert.NoError(t, client.sendHello(ctx, []byte("secret")))
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
			play: func(t *testing.T, ctx context.Context, client, server *testConnection) {
				assert.NoError(t, client.sendHello(ctx, []byte("secret")))
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
			play: func(t *testing.T, ctx context.Context, client, server *testConnection) {
				assert.NoError(t, client.sendHello(ctx, nil))
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
			play: func(t *testing.T, ctx context.Context, client, server *testConnection) {
				assert.NoError(t, client.sendHello(ctx, nil))
				assertReceive(t, server, &message.Hello{Nickname: "client"})
				assertReceive(t, client, &message.Welcome{Nickname: "server"})

				assert.NoError(t, client.GetFile(ctx, "foo.exe", "dir"))
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
				"bar.txt":     "bar",
				"dir/foo.exe": "foo",
			},
		},
		{
			name: "file upload",
			play: func(t *testing.T, ctx context.Context, client, server *testConnection) {
				assert.NoError(t, client.sendHello(ctx, nil))
				assertReceive(t, server, &message.Hello{Nickname: "client"})
				assertReceive(t, client, &message.Welcome{Nickname: "server"})

				assert.NoError(t, client.AskToPutFile(ctx, "baz.txt", "dir"))
				assertReceive(t, server, &message.PutFile{
					Path:        "baz.txt",
					Destination: "dir",
				})

				assert.NoError(t, server.AgreeToPutFile(ctx, "baz.txt", "dir"))
				assertReceive(t, client, &message.PutFileAgree{
					PutFile: message.PutFile{
						Path:        "baz.txt",
						Destination: "dir",
					},
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
				"baz.txt": "baz 2",
			},
			finishedServerFS: map[string]string{
				"foo.exe":     "foo",
				"bar.txt":     "bar",
				"baz.txt":     "baz",
				"dir/baz.txt": "baz 2",
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

			assertChannelClosed(t, server.recv)
			assertChannelClosed(t, client.recv)

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

type testConnection struct {
	*Connection
	recv <-chan message.Message
}

func startServerClientPair(t *testing.T, opts testOpts) (ctx context.Context, server, client *testConnection) {
	logger := slogt.New(t)

	serverConn, clientConn := net.Pipe()

	serverPeer := &connectionPeerStub{
		nickname:   "server",
		filesystem: opts.serverFS,
		secret:     opts.secret,
		conns:      newConnectionManager(),
	}

	clientPeer := &connectionPeerStub{
		nickname:   "client",
		filesystem: opts.clientFS,
		conns:      newConnectionManager(),
	}

	ctx, cancel := context.WithCancel(context.Background())

	serverInstance, err := newConnection(serverConn, serverPeer, logger)
	assert.NoError(t, err)

	clientInstance, err := newConnection(clientConn, clientPeer, logger)
	assert.NoError(t, err)

	server = &testConnection{serverInstance, serverInstance.Recv.Listen().C}
	client = &testConnection{clientInstance, clientInstance.Recv.Listen().C}

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

func assertReceive(t *testing.T, conn *testConnection, msg message.Message) {
	t.Helper()
	got, ok := <-conn.recv
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
	conns      *connectionManager
}

func (p *connectionPeerStub) Filesystem() billy.Filesystem {
	return p.filesystem
}

func (p *connectionPeerStub) Nickname() string {
	return p.nickname
}

func (p *connectionPeerStub) CompareSecret(got []byte) bool {
	return bytes.Equal(p.secret, got)
}

func (p *connectionPeerStub) ConnectionManager() *connectionManager {
	return p.conns
}
