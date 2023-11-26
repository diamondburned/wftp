package wftp

import (
	"io"
)

type receivingFile struct {
	file   io.WriteCloser
	path   string
	buffer []byte
}

func newReceivingFile(file io.WriteCloser, path string, bufferSize uint32) *receivingFile {
	return &receivingFile{
		file:   file,
		path:   path,
		buffer: make([]byte, bufferSize),
	}
}
