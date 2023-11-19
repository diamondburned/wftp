package wftp

import (
	"net"
	"slices"
	"strings"
	"sync"
)

// ConnectionManager manages all connections to other peers.
// It only exposes methods to read connections.
type ConnectionManager struct {
	mu      sync.RWMutex
	conns   map[net.Conn]*Connection
	connIDs map[string]*Connection
}

func newConnectionManager() *ConnectionManager {
	return &ConnectionManager{
		conns:   make(map[net.Conn]*Connection),
		connIDs: make(map[string]*Connection),
	}
}

func (cm *ConnectionManager) add(conn *Connection) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	cm.conns[conn.conn] = conn
	for _, id := range conn.ids {
		cm.connIDs[id] = conn
	}
}

func (cm *ConnectionManager) addNick(conn *Connection, nick string) bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if _, ok := cm.connIDs[nick]; ok {
		return false
	}
	if _, ok := cm.conns[conn.conn]; !ok {
		return false
	}

	cm.connIDs[nick] = conn
	conn.ids = append(conn.ids, nick)
	return true
}

func (cm *ConnectionManager) remove(conn *Connection) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	delete(cm.conns, conn.conn)
	for _, id := range conn.ids {
		delete(cm.connIDs, string(id))
	}
}

// ConnectionInfo contains information about a connection.
type ConnectionInfo struct {
	RemoteAddr string
	Nicknames  []string
}

// ListConnections returns a list of all connections.
func (cm *ConnectionManager) ListConnections() []ConnectionInfo {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	infos := make([]ConnectionInfo, 0, len(cm.conns))
	for conn, connInfo := range cm.conns {
		infos = append(infos, ConnectionInfo{
			RemoteAddr: conn.RemoteAddr().String(),
			Nicknames:  connInfo.ids,
		})
	}

	slices.SortFunc(infos, func(a, b ConnectionInfo) int {
		return strings.Compare(a.RemoteAddr, b.RemoteAddr)
	})
	return infos
}
