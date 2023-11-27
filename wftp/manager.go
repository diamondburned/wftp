package wftp

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ConnectionInfo contains information about a connection.
type ConnectionInfo struct {
	// Addr is the remote address of the connection.
	Addr string
	// ServerName is the server-advertised name of the connection.
	ServerName string
	// Nickname is a user-defined nickname of the connection.
	Nickname string
}

// String implements the fmt.Stringer interface.
func (info ConnectionInfo) String() string {
	name := info.Addr
	if info.ServerName != "" {
		name = fmt.Sprintf(
			"%s (%s)",
			info.ServerName,
			info.Addr)
	}
	if info.Nickname != "" {
		name = fmt.Sprintf(
			"%s (%s %s)",
			info.Nickname,
			info.ServerName,
			info.Addr)
	}
	return name
}

// Name returns the name of the connection in the order of nickname, server
// name, then address, whichever is available first.
func (info ConnectionInfo) Name() string {
	if info.Nickname != "" {
		return info.Nickname
	}
	if info.ServerName != "" {
		return info.ServerName
	}
	return info.Addr
}

// connectionManager manages all connections to other peers.
// It only exposes methods to read connections.
type connectionManager struct {
	mu    sync.RWMutex
	conns map[*Connection]managedConnection
	nicks map[string]*Connection

	// helloed tracks which connections we have sent a hello to.
	// helloed map[*Connection]bool
}

type managedConnection struct {
	*Connection
	info  ConnectionInfo
	added time.Time
}

func newConnectionManager() *connectionManager {
	return &connectionManager{
		conns: make(map[*Connection]managedConnection),
		nicks: make(map[string]*Connection),
	}
}

func (cm *connectionManager) add(conn *Connection) bool {
	managedConn := managedConnection{
		Connection: conn,
		info:       ConnectionInfo{Addr: conn.conn.RemoteAddr().String()},
		added:      time.Now(),
	}

	cm.mu.Lock()
	defer cm.mu.Unlock()

	_, exists := cm.conns[conn]
	if exists {
		return false
	}

	_, exists = cm.nicks[managedConn.info.Addr]
	if exists {
		return false
	}

	cm.conns[conn] = managedConn
	cm.nicks[managedConn.info.Addr] = conn

	return true
}

func (cm *connectionManager) setNick(conn *Connection, nick string, kind nicknameKind) bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	managedConn, ok := cm.conns[conn]
	if !ok {
		return false
	}

	_, exists := cm.nicks[nick]
	if exists {
		return false
	}

	switch kind {
	case serverAdvertisedNickname:
		managedConn.info.ServerName = nick
	case customUserNickname:
		managedConn.info.Nickname = nick
	default:
		return false
	}

	cm.conns[conn] = managedConn
	cm.nicks[nick] = conn

	return true
}

func (cm *connectionManager) remove(conn *Connection) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	managedConn, ok := cm.conns[conn]
	if !ok {
		return
	}

	delete(cm.conns, conn)
	delete(cm.nicks, managedConn.info.Addr)
	delete(cm.nicks, managedConn.info.Nickname)
	delete(cm.nicks, managedConn.info.ServerName)
}

// list returns a list of all connections.
func (cm *connectionManager) list() []*Connection {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	conns := make([]*Connection, 0, len(cm.conns))
	for conn := range cm.conns {
		conns = append(conns, conn)
	}
	return conns
}

func (cm *connectionManager) find(nick string) *Connection {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	return cm.nicks[nick]
}

func (cm *connectionManager) close() error {
	var errs []error
	// Copy the list of connections so we can close them without holding the
	// lock to avoid deadlocks.
	for _, conn := range cm.list() {
		if err := conn.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func (cm *connectionManager) connInfo(conn *Connection) ConnectionInfo {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	return cm.conns[conn].info
}
