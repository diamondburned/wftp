package wftp

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

type fileTransferPool struct {
	mu     sync.RWMutex
	writes map[uint32]*fileWriter
	lastID uint32
}

type fileWriter struct {
	id      uint32
	size    uint64
	dst     io.WriteCloser
	buffer  []byte
	written atomic.Uint64
}

func newFileTransferPool() *fileTransferPool {
	return &fileTransferPool{
		writes: make(map[uint32]*fileWriter),
	}
}

func (ftp *fileTransferPool) closeAll() {
	for _, ft := range ftp.writes {
		ft.dst.Close()
	}
}

// add reserves a new ID for a file transfer.
func (ftp *fileTransferPool) add() uint32 {
	ftp.mu.Lock()
	defer ftp.mu.Unlock()

	id := ftp.lastID
	ftp.lastID++
	return id
}

// addWriter adds a new file transfer to the pool. It returns the ID of the transfer.
func (ftp *fileTransferPool) addWriter(dst io.WriteCloser, size uint64, bufferSize uint32) *fileWriter {
	ftp.mu.Lock()
	defer ftp.mu.Unlock()

	ft := &fileWriter{
		id:     ftp.lastID,
		dst:    dst,
		size:   size,
		buffer: make([]byte, bufferSize),
	}

	ftp.lastID++
	ftp.writes[ft.id] = ft
	return ft
}

func (ftp *fileTransferPool) getWriter(id uint32) *fileWriter {
	ftp.mu.RLock()
	defer ftp.mu.RUnlock()

	return ftp.writes[id]
}

func (ftp *fileTransferPool) remove(id uint32) {
	ftp.mu.Lock()
	defer ftp.mu.Unlock()

	delete(ftp.writes, id)
}

func (ft *fileWriter) Write(chunk []byte) (uint32, error) {
	written := ft.written.Add(uint64(len(chunk)))
	overflowed := written > ft.size

	if overflowed {
		overflow := written - ft.size
		chunk = chunk[:len(chunk)-int(overflow)]
	}

	n, err := ft.dst.Write(chunk)
	if err != nil {
		return 0, fmt.Errorf("could not write chunk: %v", err)
	}

	if overflowed {
		return uint32(n), fmt.Errorf("too many bytes written")
	}

	return uint32(n), nil
}

func (fw *fileWriter) Close() error {
	return fw.dst.Close()
}

func (fw *fileWriter) isDone() bool {
	return fw.written.Load() >= fw.size
}
