package wftp

import (
	"slices"
	"strings"
	"sync"
	"time"
)

// ConnectionManager manages all connections to other peers.
// It only exposes methods to read connections.
type ConnectionManager struct {
	mu      sync.RWMutex
	conns   map[*Connection]managedConnection
	connIDs map[string]*Connection

	// helloed tracks which connections we have sent a hello to.
	// helloed map[*Connection]bool
}

type managedConnection struct {
	*Connection
	added time.Time
}

func newConnectionManager() *ConnectionManager {
	return &ConnectionManager{
		conns:   make(map[*Connection]managedConnection),
		connIDs: make(map[string]*Connection),
	}
}

func (cm *ConnectionManager) add(conn *Connection) {
	if len(conn.nicks) == 0 {
		panic("connection must have at least one id")
	}

	managedConnection := managedConnection{
		Connection: conn,
		added:      time.Now(),
	}

	cm.mu.Lock()
	defer cm.mu.Unlock()

	cm.conns[conn] = managedConnection
	for id := range conn.nicks {
		cm.connIDs[id] = conn
	}
}

func (cm *ConnectionManager) addNick(conn *Connection, nick string) bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	existingConn, ok := cm.conns[conn]
	if ok && existingConn.Connection != conn {
		// Already have another connection with this nick.
		return false
	}

	cm.connIDs[nick] = conn
	return true
}

func (cm *ConnectionManager) remove(conn *Connection) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	delete(cm.conns, conn)
	for id := range conn.nicks {
		delete(cm.connIDs, id)
	}
}

// ConnectionInfo contains information about a connection.
type ConnectionInfo struct {
	// RemoteAddr is the remote address of the connection.
	RemoteAddr string
	// ServerName is the server-advertised name of the connection.
	ServerName string
	// Nicknames is a list of nicknames of the connection.
	// This only contains user-defined nicknames.
	Nicknames []string
}

// ListConnections returns a list of all connections.
func (cm *ConnectionManager) ListConnections() []ConnectionInfo {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	infos := make([]ConnectionInfo, 0, len(cm.conns))
	for conn := range cm.conns {
		infos = append(infos, conn.connectionInfo())
	}

	slices.SortFunc(infos, func(a, b ConnectionInfo) int {
		return strings.Compare(a.RemoteAddr, b.RemoteAddr)
	})

	return infos
}
