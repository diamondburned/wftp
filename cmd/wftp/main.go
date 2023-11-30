package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"text/tabwriter"

	"github.com/Pallinder/go-randomdata"
	"github.com/chzyer/readline"
	"github.com/dustin/go-humanize"
	"github.com/google/shlex"
	"github.com/lmittmann/tint"
	"github.com/spf13/pflag"
	"golang.org/x/sync/errgroup"
	"libdb.so/wftp"
	"libdb.so/wftp/message"
)

var (
	listenAddr        = ""
	currentDir        = "."
	nickname          = ""
	secret            = ""
	daemon            = false
	verbose           = false
	jsonLog           = false
	autoAcceptUploads = false
)

func init() {
	nickname = randomName()

	pflag.StringVarP(&listenAddr, "listen", "l", listenAddr, "listen address if this is a listening peer")
	pflag.StringVarP(&currentDir, "dir", "D", currentDir, "working directory")
	pflag.StringVarP(&nickname, "nickname", "n", nickname, "custom nickname, otherwise generate one")
	pflag.StringVarP(&secret, "secret", "s", secret, "secret passphrase to expect, optional")
	pflag.BoolVarP(&daemon, "daemon", "d", daemon, "run as daemon (no CLI)")
	pflag.BoolVarP(&verbose, "verbose", "v", verbose, "verbose logging")
	pflag.BoolVar(&jsonLog, "json", jsonLog, "log in JSON format")
	pflag.BoolVar(&autoAcceptUploads, "auto-accept-uploads", autoAcceptUploads, "automatically accept uploads, implied if daemon is true")
}

func main() {
	log.SetFlags(0)
	pflag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func run(ctx context.Context) (err error) {
	var read *readline.Instance
	logOutput := io.Writer(os.Stderr)

	configDir, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("could not get user config dir: %w", err)
	}
	configDir = filepath.Join(configDir, "wftp")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return fmt.Errorf("could not create config dir: %w", err)
	}

	if !daemon {
		read, err = readline.NewEx(&readline.Config{
			Prompt:      nickname + "> ",
			HistoryFile: filepath.Join(configDir, "history"),
		})
		if err != nil {
			return fmt.Errorf("could not create readline: %w", err)
		}
		defer read.Close()
		logOutput = read.Stderr()
	}

	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}

	var logHandler slog.Handler
	if jsonLog {
		logHandler = slog.NewJSONHandler(logOutput, &slog.HandlerOptions{
			Level: level,
		})
	} else {
		// Support colors on Windows.
		logHandler = tint.NewHandler(logOutput, &tint.Options{
			Level:      level,
			TimeFormat: "15:04:05 PM", // extended time.Kitchen
			// Disable colors if the output is not a terminal.
			NoColor: !readline.DefaultIsTerminal(),
		})
	}

	logger := slog.New(logHandler)
	slog.SetDefault(logger)

	peer, err := wftp.NewPeer(wftp.Opts{
		CurrentDir: currentDir,
		Nickname:   nickname,
		Logger:     logger,
		Secret:     []byte(secret),
	})
	if err != nil {
		return fmt.Errorf("could not create peer: %w", err)
	}
	defer peer.Close()

	if listenAddr != "" {
		listening, err := peer.Listen(ctx, listenAddr)
		if err != nil {
			return fmt.Errorf("could not listen: %w", err)
		}
		defer listening.Close()

		logger.InfoContext(ctx,
			"listening to incoming connections",
			"addr", listening.Addr)
	}

	rl := &readlineInstance{
		Instance: read,
		peer:     peer,
		logger:   logger,
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errg, ctx := errgroup.WithContext(ctx)

	errg.Go(func() error {
		if daemon {
			return nil
		}

		rl.refreshPrompt()
		for {
			line, err := read.Readline()
			if err != nil {
				if errors.Is(err, readline.ErrInterrupt) {
					cancel()
					return nil
				}
				return fmt.Errorf("could not read line: %w", err)
			}

			args, err := shlex.Split(line)
			if err != nil {
				rl.logger.Error("could not handle line", "error", err)
				continue
			}

			if len(args) == 0 {
				continue
			}

			if err := rl.run(ctx, args[0], args[1:]); err != nil {
				if errors.Is(err, readline.ErrInterrupt) {
					cancel()
				}
				rl.logger.Error("could not handle line", "error", err)
			}
		}
	})

	errg.Go(func() error {
		msgListener := rl.peer.Recv.Listen()
		for {
			var connMsg wftp.ConnectionMessage
			var ok bool

			select {
			case <-ctx.Done():
				return ctx.Err()
			case connMsg, ok = <-msgListener.C:
				if !ok {
					return nil
				}
			}

			switch msg := connMsg.Message.(type) {
			case *wftp.SystemConnectionAdded:
				// New connection, so replace the active connection if none.
				rl.active.CompareAndSwap(nil, connMsg.Connection)
				rl.refreshPrompt()

			case *wftp.SystemConnectionTerminated:
				// Connection closed, so remove it if it's the active
				// connection.
				rl.active.CompareAndSwap(connMsg.Connection, nil)
				rl.refreshPrompt()

			case *message.Chat:
				rl.logger.Info(
					"",
					"peer", connMsg.Connection.Info(),
					"message", msg.Message)

			case *message.DirectoryList:
				var str strings.Builder
				fmt.Fprintf(&str,
					"contents of directory %s (total %d):\n",
					msg.Path,
					len(msg.Entries))

				w := tabwriter.NewWriter(&str, 0, 0, 1, ' ', 0)
				for _, entry := range msg.Entries {
					size := humanize.Bytes(entry.Size)
					fmt.Fprintf(w, " %s\t%s\t%s\n", entry.Mode, size, entry.Name)
				}
				w.Flush()

				rl.logger.Info(
					str.String(),
					"peer", connMsg.Connection.Info())

			case *message.GetFileAgree:
				rl.logger.Info(
					"downloading file",
					"peer", connMsg.Connection.Info(),
					"path", msg.Path)

			case *message.PutFile:
				if autoAcceptUploads || daemon {
					if err := connMsg.Connection.AgreeToPutFile(ctx,
						string(msg.Path),
						string(msg.Destination)); err != nil {

						rl.logger.Error(
							"could not automatically agree to upload",
							"peer", connMsg.Connection.Info(),
							"error", err)
					}
				} else {
					rl.logger.Info(
						"peer wants to upload to you, use 'accept <path> [destination]' to accept",
						"peer", connMsg.Connection.Info(),
						"path", msg.Path,
						"destination", msg.Destination)
				}
			}
		}
	})

	return errg.Wait()
}

type readlineInstance struct {
	*readline.Instance
	peer   *wftp.Peer
	logger *slog.Logger
	active atomic.Pointer[wftp.Connection] // nil if no active connection
}

type pendingUploadKey struct {
	connection *wftp.Connection
	filePath   message.FilePath
}

func (rl *readlineInstance) currentPrompt() string {
	conn := rl.active.Load()
	if conn == nil {
		return fmt.Sprintf("[%s] ", nickname)
	}
	return fmt.Sprintf("[%s]~>(%s) ", nickname, conn.Info().Name())
}

func (rl *readlineInstance) refreshPrompt() {
	rl.Instance.SetPrompt(rl.currentPrompt())
	rl.Instance.Refresh()
}

var errNoActiveConnection = errors.New("no active peer, use 'connect' to connect to a peer")

func (rl *readlineInstance) run(ctx context.Context, arg0 string, argv []string) error {
	switch arg0 {
	case "connect":
		if len(argv) < 1 {
			return fmt.Errorf("usage: connect <addr> [-s]")
		}

		var secret []byte
		if len(argv) > 1 && argv[len(argv)-1] == "-s" {
			var err error

			cfg := rl.GenPasswordConfig()
			cfg.Prompt = fmt.Sprintf("[%s]~>(%s) secret: ", nickname, argv[0])
			cfg.EnableMask = true
			cfg.MaskRune = '*'

			secret, err = rl.ReadPasswordWithConfig(cfg)
			if err != nil {
				return fmt.Errorf("could not read secret: %w", err)
			}
		}

		conn, err := rl.peer.Connect(ctx, argv[0], secret)
		if err != nil {
			return fmt.Errorf("could not connect: %w", err)
		}

		rl.active.Store(conn)
		rl.refreshPrompt()

		return nil

	case "peers":
		conns := rl.peer.Connections()
		infos := make([]string, len(conns))
		for i, conn := range conns {
			infos[i] = conn.Info().String()
		}

		rl.logger.InfoContext(ctx,
			"found peers",
			"count", len(conns),
			"peers", infos)

		return nil

	case "nick":
		var conn *wftp.Connection
		var nick string

		switch len(argv) {
		case 1:
			conn = rl.active.Load()
			nick = argv[0]
		case 2:
			conn = rl.peer.FindConnection(argv[0])
			nick = argv[1]
		default:
			return fmt.Errorf("usage: nick [peer] <nickname>")
		}

		if conn == nil {
			return fmt.Errorf("could not find peer or no active peer")
		}

		if !isValidNickname(nick) {
			return fmt.Errorf("invalid nickname")
		}

		if !conn.SetNickname(nick) {
			return fmt.Errorf("nickname already exists")
		}
		return nil

	case "use":
		if len(argv) != 1 {
			return fmt.Errorf("usage: use <peer>")
		}

		conn := rl.peer.FindConnection(argv[0])
		if conn == nil {
			return fmt.Errorf("could not find peer")
		}

		rl.active.Store(conn)
		rl.refreshPrompt()

		return nil

	case "broadcast":
		if len(argv) != 1 {
			return fmt.Errorf("usage: broadcast <message>")
		}

		msg := strings.Join(argv, " ")
		if err := rl.peer.BroadcastChat(ctx, msg); err != nil {
			return fmt.Errorf("could not broadcast: %w", err)
		}

		rl.logger.Info(
			"",
			"peer", nickname,
			"message", msg)

		return nil

	case "active", "send", "list", "ls", "get", "put", "pending", "accept":
		// These commands require an active connection.
		break

	case "help", "h":
		var str strings.Builder
		str.WriteString("commands:\n")

		w := tabwriter.NewWriter(&str, 0, 0, 2, ' ', 0)
		fmt.Fprintf(w, " %s\t%s\t%s\n", "connect", "<addr> [-s]", "connect to a peer")
		fmt.Fprintf(w, " %s\t%s\t%s\n", "peers", "", "list all connected peers")
		fmt.Fprintf(w, " %s\t%s\t%s\n", "broadcast", "<message>", "send a chat message to all peers")
		fmt.Fprintf(w, " %s\t%s\t%s\n", "nick", "[peer] <nick>", "set your nickname")
		fmt.Fprintf(w, " %s\t%s\t%s\n", "active", "", "print current active peer")
		fmt.Fprintf(w, " %s\t%s\t%s\n", "send", "<peer> <message>", "send a chat message to a peer")
		fmt.Fprintf(w, " %s\t%s\t%s\n", "list, ls", "", "list files")
		fmt.Fprintf(w, " %s\t%s\t%s\n", "use", "<peer>", "use a peer for further commands")
		fmt.Fprintf(w, " %s\t%s\t%s\n", "get", "<path> [dir]", "download a file")
		fmt.Fprintf(w, " %s\t%s\t%s\n", "put", "<path> [dir]", "upload a file")
		fmt.Fprintf(w, " %s\t%s\t%s\n", "pending", "", "list pending uploads")
		fmt.Fprintf(w, " %s\t%s\t%s\n", "accept", "<path> [dir]", "accept a pending upload")
		fmt.Fprintf(w, " %s\t%s\t%s\n", "help, h", "", "show this help")
		fmt.Fprintf(w, " %s\t%s\t%s\n", "exit", "", "exit the program")

		w.Flush()
		log.Println(str.String())
		return nil

	case "exit":
		return readline.ErrInterrupt

	default:
		return fmt.Errorf("unknown command: %s", arg0)
	}

	conn := rl.active.Load()
	if conn == nil {
		return errNoActiveConnection
	}

	switch arg0 {
	case "active":
		rl.logger.InfoContext(ctx,
			"active peer",
			"peer", conn.Info().String())

		return nil

	case "send":
		if len(argv) < 1 {
			return fmt.Errorf("usage: chat <message>")
		}

		msg := strings.Join(argv, " ")
		if err := conn.SendChat(ctx, msg); err != nil {
			return fmt.Errorf("could not send chat message: %w", err)
		}

		rl.logger.Info(
			"",
			"peer", nickname,
			"message", msg)

		return nil

	case "list", "ls":
		var dir string
		switch len(argv) {
		case 0:
			dir = "."
		case 1:
			dir = argv[0]
		default:
			return fmt.Errorf("usage: list [dir]")
		}

		if err := conn.ListDirectory(ctx, dir); err != nil {
			return fmt.Errorf("could not request directory: %w", err)
		}

		rl.logger.Debug(
			"asked peer to list directory",
			"peer", conn.Info(),
			"directory", dir)

		return nil

	case "get":
		if len(argv) < 1 || len(argv) > 2 {
			return fmt.Errorf("usage: get <path> [destination directory]")
		}

		path := argv[0]
		dstDir := ""
		if len(argv) == 2 {
			dstDir = argv[1]
		}

		if err := conn.GetFile(ctx, path, dstDir); err != nil {
			return fmt.Errorf("could not get file: %w", err)
		}

		rl.logger.Debug(
			"asked peer to download file",
			"peer", conn.Info(),
			"path", path,
			"destination", dstDir)

		return nil

	case "put":
		if len(argv) < 1 || len(argv) > 2 {
			return fmt.Errorf("usage: put <path> [destination directory]")
		}

		path := argv[0]
		dstDir := ""
		if len(argv) == 2 {
			dstDir = argv[1]
		}

		if err := conn.AskToPutFile(ctx, path, dstDir); err != nil {
			return fmt.Errorf("could not ask to put file: %w", err)
		}

		rl.logger.Info(
			"asked peer to upload file, now waiting for approval",
			"peer", conn.Info(),
			"path", path,
			"destination", dstDir)

		return nil

	case "pending":
		requests := conn.PendingPutRequests()
		rl.logger.Info(
			"peer has requested to upload these files",
			"peer", conn.Info(),
			"requests", requests)

		return nil

	case "accept":
		if len(argv) < 1 || len(argv) > 2 {
			return fmt.Errorf("usage: accept <path> [destination directory]")
		}

		path := argv[0]
		dstDir := ""
		if len(argv) == 2 {
			dstDir = argv[1]
		}

		if err := conn.AgreeToPutFile(ctx, path, dstDir); err != nil {
			return fmt.Errorf("could not agree to put file: %w", err)
		}

		return nil

	default:
		panic("unreachable")
	}
}

var validNicknameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

func isValidNickname(nickname string) bool {
	return validNicknameRe.MatchString(nickname)
}

func randomName() string {
	return fmt.Sprintf("%s-%s",
		strings.ToLower(randomdata.Adjective()),
		strings.ToLower(randomdata.Noun()))
}
