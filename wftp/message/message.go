// Package message defines the messages that are sent between peers for a worse
// file transfer protocol (wftp). The messages are encoded using the binary
// package.
//
// To see how messages are encoded, see the documentation for the ServerMessage
// interface.
//
// # Model
//
// This protocol doesn't assume a server-client model. Instead, it assumes a
// peer-to-peer model. This means that any peer may send or receive any files
// from any other peer.
//
// However, throughout the documentation, the terms "server" and "client" are
// used to refer to the two peers. In this case, the client wishes to connect to
// the server. The server will be listening for connections from the client.
// After handshaking, they both operate as peers.
package message

import (
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
)

// Endianness is the endianness used to encode and decode messages.
var Endianness = binary.BigEndian

// Message is a message sent from one peer to another.
//
// # Encoding
//
// These messages are meant to be encoded as binary to be sent over TCP. Each
// message is prefixed with a 1-byte message type. The message type is followed
// by the message payload. The length of the payload is variable and depends on
// the message type.
//
// The following is a diagram of the message format:
//
//	+--------+-----------------+
//	|  Type  |     Payload     |
//	+--------+-----------------+
//	| 1 byte | Variable length |
//	+--------+-----------------+
//
// For documentation regarding how messages are encoded, see the documentation
// for that message type.
//
// ## Scalar Types
//
// This section defines several scalar types for use in the documentation.
// The documentation for each message type will use these types.
//
//   - `uint8`: 1 byte unsigned integer
//   - `uint16`: 2 byte unsigned integer, big endian
//   - `uint32`: 4 byte unsigned integer, big endian
//   - `uint64`: 8 byte unsigned integer, big endian
//   - `string`: a `uint32` length followed by that many bytes
//   - `[]byte`: encoded exactly as `string`
//   - `bool`: 1 byte, 0x00 for false, 0x01 for true, any other value is
//     undefined
//   - `[]T`: a `uint32` length followed by that many `T`s. `T` is any type that
//     the message defines, which may or may not be a scalar type.
//
// ## Aggregate Types
//
// A message may be defined as an aggregate type and may even contain other
// aggregate types. This section defines the encoding of aggregate types.
//
// An aggregate type is represented as a Go struct in this package. Each field
// is encoded in the order that they are defined in the struct. The encoding of
// each field is defined by the type of the field:
//
//   - If the field is a scalar type, then the encoding is defined by the scalar
//     type.
//   - If the field is an aggregate type, then the encoding is defined by the
//     aggregate type.
//
// For example, consider the following struct:
//
//	type Example struct {
//	    A uint8
//	    B uint16
//	    C string
//	}
//
// The encoding of this struct is defined as follows:
//
//	+--------+---------+-----------------+
//	| 1 byte | 2 bytes | Variable length |
//	+--------+---------+-----------------+
//
// The names are intentionally omitted from the diagram because they are not
// encoded. Instead, the values are encoded in the order that they are defined
// in the struct. It is expected that the receiver of the message knows the
// order of the fields.
//
// Be extremely careful: if the order of the fields is changed, then the
// encoding will change. This will break compatibility with any existing
// implementations. As a result, it is recommended that any breaking changes
// to the protocol be done by creating a new message type, and that the old
// message type be deprecated.
type Message interface {
	// Type returns the type of message.
	Type() MessageType
	// Encode encodes the message to wire protocol.
	// It must not encode the message type.
	Encode(w io.Writer) error
	// Decode decodes the message from wire protocol.
	// It must not decode the message type.
	Decode(r io.Reader) error
}

// ReadType reads a message type from the reader. It reads exactly one byte from
// the reader. If the reader returns an error, then the message type is
// undefined.
func ReadType(r io.Reader) (MessageType, error) {
	var tbuf [1]byte
	if _, err := io.ReadFull(r, tbuf[:]); err != nil {
		return 0, err
	}
	return MessageType(tbuf[0]), nil
}

// Read reads a message from the reader. It reads exactly one message from the
// reader. If the reader returns an error, then the message is undefined.
func Read(r io.Reader) (Message, error) {
	t, err := ReadType(r)
	if err != nil {
		return nil, err
	}

	var m Message
	switch t {
	case MessageTypeHello:
		m = &Hello{}
	case MessageTypeWelcome:
		m = &Welcome{}
	case MessageTypeTerminate:
		m = &Terminate{}
	case MessageTypeError:
		m = &Error{}
	case MessageTypeListDirectory:
		m = &ListDirectory{}
	case MessageTypeDirectoryList:
		m = &DirectoryList{}
	case MessageTypeGetFile:
		m = &GetFile{}
	case MessageTypeFileTransferBegin:
		m = &FileTransferBegin{}
	case MessageTypeFileTransferData:
		m = &FileTransferData{}
	case MessageTypeFileUpload:
		m = &FileUpload{}
	case MessageTypeFileUploadAgree:
		m = &FileUploadAgree{}
	case MessageTypeFileUploadData:
		m = &FileUploadData{}
	default:
		return nil, fmt.Errorf("unknown message type: %d", t)
	}

	if err := m.Decode(r); err != nil {
		return nil, err
	}

	return m, nil
}

// Write writes the message in wire protocol to the writer. It writes exactly
// one message to the writer. If the writer returns an error, then the message
// is undefined.
func Write(w io.Writer, msg Message) error {
	if _, err := w.Write([]byte{byte(msg.Type())}); err != nil {
		return err
	}
	return msg.Encode(w)
}

// EncodableMessage is a message that can be encoded and decoded.
type EncodableMessage interface {
	Encode(w io.Writer) (int, error)
	Decode(r io.Reader) (int, error)
}

// MessageType is the type of message sent.
type MessageType uint8

const (
	MessageTypeHello     MessageType = 1
	MessageTypeWelcome   MessageType = 2
	MessageTypeTerminate MessageType = 3
	MessageTypeError     MessageType = 4

	MessageTypeListDirectory MessageType = 10
	MessageTypeDirectoryList MessageType = 11

	MessageTypeGetFile           MessageType = 20
	MessageTypeFileTransferBegin MessageType = 21
	MessageTypeFileTransferData  MessageType = 22
	MessageTypeFileTransferEnd   MessageType = 23

	MessageTypeFileUpload      MessageType = 30
	MessageTypeFileUploadAgree MessageType = 31
	MessageTypeFileUploadData  MessageType = 32
	MessageTypeFileUploadEnd   MessageType = 33
)

func (t MessageType) String() string {
	switch t {
	case MessageTypeHello:
		return "Hello"
	case MessageTypeWelcome:
		return "Welcome"
	case MessageTypeTerminate:
		return "Terminate"
	case MessageTypeError:
		return "Error"
	case MessageTypeListDirectory:
		return "ListDirectory"
	case MessageTypeDirectoryList:
		return "DirectoryList"
	case MessageTypeGetFile:
		return "GetFile"
	case MessageTypeFileTransferBegin:
		return "FileTransferBegin"
	case MessageTypeFileTransferData:
		return "FileTransferData"
	case MessageTypeFileTransferEnd:
		return "FileTransferEnd"
	case MessageTypeFileUpload:
		return "FileUpload"
	case MessageTypeFileUploadAgree:
		return "FileUploadAgree"
	case MessageTypeFileUploadData:
		return "FileUploadData"
	case MessageTypeFileUploadEnd:
		return "FileUploadEnd"
	default:
		return fmt.Sprintf("MessageType(%d)", t)
	}
}

// Hello is a message sent from the peer to the client to
// indicate that the client has successfully connected to the server.
// This is always the first message sent from the server to the client.
//
// # Encoding
//
// The Hello message is zero bytes long.
type Hello struct {
	// Nickname is a self-identifying nickname of the peer.
	// It may be used by the user as an alias for the actual IP address.
	Nickname string
	// Secret is a secret that the peer may use to authenticate to the other
	// peer. It is assumed that the secret is known by both peers.
	// If the secret is empty, then the peer does not wish to authenticate.
	Secret []byte
}

func (m *Hello) Type() MessageType {
	return MessageTypeHello
}

func (m *Hello) Encode(w io.Writer) error {
	return encodeBinary(w, []any{
		m.Nickname,
		m.Secret,
	})
}

func (m *Hello) Decode(r io.Reader) error {
	return decodeBinary(r, []any{
		&m.Nickname,
		&m.Secret,
	})
}

// Welcome is a message sent from the peer to the client to indicate that the
// peer has successfully connected to the server. This is always the first
// message sent from the server to the client.
type Welcome struct {
	// Nickname is a self-identifying nickname of the peer.
	// It may be used by the user as an alias for the actual IP address.
	Nickname string
}

func (m *Welcome) Type() MessageType {
	return MessageTypeWelcome
}

func (m *Welcome) Encode(w io.Writer) error {
	return encodeBinary(w, []any{
		m.Nickname,
	})
}

func (m *Welcome) Decode(r io.Reader) error {
	return decodeString(r, &m.Nickname)
}

// Terminate is a message sent from a peer to another to indicate that the peer
// wishes to be disconnected. This is always the last message sent from the
// peer. It signals the other peer to cancel any outstanding requests from the
// peer.
//
// Note that any peer may send a Terminate message at any time. If a peer
// receives a Terminate message after it has already sent a Terminate message,
// then it must also send a Terminated message. Vice versa, if a peer sent a
// Terminate message and receives a Terminate message, then it must send a
// Terminated message.
//
// # Encoding
//
// The Terminate message is zero bytes long.
type Terminate struct{}

func (m *Terminate) Type() MessageType {
	return MessageTypeTerminate
}

func (m *Terminate) Encode(w io.Writer) error {
	return nil
}

func (m *Terminate) Decode(r io.Reader) error {
	return nil
}

// Error is a message sent from the peer to the client to indicate that an
// error has occurred. It is sent in response to a message that caused an
// error.
//
// The peer may choose to immediately terminate the connection after sending
// an Error message, indicating that the error is fatal.
type Error struct {
	// Message is the error message.
	Message string
}

func (m *Error) Type() MessageType {
	return MessageTypeError
}

func (m *Error) Encode(w io.Writer) error {
	return encodeBinary(w, []any{
		m.Message,
	})
}

func (m *Error) Decode(r io.Reader) error {
	return decodeString(r, &m.Message)
}

// ListDirectory is a message sent from the client to the server to indicate
// that the client wishes to list the contents of a directory.
type ListDirectory struct {
	// Path is the path of the directory to list.
	// It is relative to the directory that the server is serving.
	Path string
}

func (m *ListDirectory) Type() MessageType {
	return MessageTypeListDirectory
}

func (m *ListDirectory) Encode(w io.Writer) error {
	return encodeBinary(w, []any{
		m.Path,
	})
}

func (m *ListDirectory) Decode(r io.Reader) error {
	return decodeString(r, &m.Path)
}

// DirectoryList is a message sent from the server to the client
// to indicate that the client should display a list of files and directories.
// It is sent in response to a ClientListDirectory message.
//
// # Encoding
//
// The DirectoryList message starts off with the path of the directory
// encoded as a `string`.
type DirectoryList struct {
	// Path is the path of the directory that was listed.
	// It is relative to the directory that the server is serving.
	Path string
	// Entries is the list of entries in the directory.
	Entries []DirectoryEntry
}

func (m *DirectoryList) Type() MessageType {
	return MessageTypeDirectoryList
}

func (m *DirectoryList) Encode(w io.Writer) error {
	if err := encodeBinary(w, []any{
		m.Path,
		uint32(len(m.Entries)),
	}); err != nil {
		return err
	}
	for _, entry := range m.Entries {
		if err := entry.Encode(w); err != nil {
			return err
		}
	}
	return nil
}

func (m *DirectoryList) Decode(r io.Reader) error {
	if err := decodeString(r, &m.Path); err != nil {
		return err
	}

	var numEntries uint32
	if err := binary.Read(r, Endianness, &numEntries); err != nil {
		return err
	}

	entries := make([]DirectoryEntry, numEntries)
	for i := range entries {
		if err := entries[i].Decode(r); err != nil {
			return err
		}
	}

	m.Entries = entries
	return nil
}

// DirectoryEntry is a single entry in a directory listing.
// It closely resembles fs.DirEntry but is not an interface.
type DirectoryEntry struct {
	Name  string
	Mode  fs.FileMode // uint32
	IsDir bool
}

func (m *DirectoryEntry) Encode(w io.Writer) error {
	return encodeBinary(w, []any{
		m.Name,
		m.Mode,
		m.IsDir,
	})
}

func (m *DirectoryEntry) Decode(r io.Reader) error {
	if err := decodeString(r, &m.Name); err != nil {
		return err
	}

	if err := binary.Read(r, Endianness, &m.Mode); err != nil {
		return err
	}

	if err := binary.Read(r, Endianness, &m.IsDir); err != nil {
		return err
	}

	return nil
}

// GetFile is a message sent to the peer to request a file. The peer will reply
// with a FileTransferBegin message followed by one or more FileTransferData
// messages.
// It may also reply with an Error message otherwise.
type GetFile struct {
	// Path is the path of the file that was requested.
	Path string
}

func (m *GetFile) Type() MessageType {
	return MessageTypeGetFile
}

func (m *GetFile) Encode(w io.Writer) error {
	return encodeBinary(w, []any{
		m.Path,
	})
}

func (m *GetFile) Decode(r io.Reader) error {
	if err := decodeString(r, &m.Path); err != nil {
		return err
	}
	return nil
}

// FileTransferBegin is a message sent from the server to the client
// to indicate that the server will be sending a file to the client.
// It is sent in response to a GetFile message.
//
// # Encoding
//
// The FileTransferBegin message contains only scalar types and is encoded
// as a simple aggregate type.
type FileTransferBegin struct {
	// Path is the path of the file that is being transferred.
	// It is relative to the directory that the server is serving.
	Path string
	// DataID is the DataID of the file that is being transferred. Future
	// ServerMessageFileTransferData messages will contain this DataID. The
	// client should use this DataID to determine which file the data belongs to
	// and join the fragments together.
	DataID uint32
	// DataSize is a hint for the size of the data that will be sent in the
	// ServerMessageFileTransferData messages. The client should use this to
	// allocate a buffer for the data. It may be 0, in which case the client
	// should allocate a buffer of the size of the file.
	DataSize uint32
}

func (m *FileTransferBegin) Type() MessageType {
	return MessageTypeFileTransferBegin
}

func (m *FileTransferBegin) Encode(w io.Writer) error {
	return encodeBinary(w, []any{
		m.Path,
		m.DataID,
		m.DataSize,
	})
}

func (m *FileTransferBegin) Decode(r io.Reader) error {
	return decodeBinary(r, []any{
		&m.Path,
		&m.DataID,
		&m.DataSize,
	})
}

// FileTransferData is a message sent to indicate that the peer is sending over
// a fragment of a file to the client. It is sent after a
// FileTransferBegin message.
type FileTransferData struct {
	// DataID is the DataID of the file that is being transferred. This should
	// match the DataID of the ServerFileTransferBegin message.
	DataID uint32
	// Data is the fragment of the file that is being transferred.
	// It may be of any length. The client should join all fragments together
	// and determine if enough data has been received to reconstruct the file.
	Data []byte
}

func (m *FileTransferData) Type() MessageType {
	return MessageTypeFileTransferData
}

func (m *FileTransferData) Encode(w io.Writer) error {
	return encodeBinary(w, []any{
		m.DataID,
		m.Data,
	})
}

// Decode reads a FileTransferData message from r.
// If m.Data is not large enough to hold the data, it will be reallocated,
// otherwise it will be reused. As such, it is recommended to reuse the same
// ServerFileTransferData instance per data ID.
func (m *FileTransferData) Decode(r io.Reader) error {
	if err := binary.Read(r, Endianness, &m.DataID); err != nil {
		return err
	}
	if err := decodeBytesBuf(r, &m.Data); err != nil {
		return err
	}
	return nil
}

// FileTransferEnd is a message sent to indicate that the peer has finished
// sending a file to the client. It is sent after some FileTransferData
// messages.
type FileTransferEnd struct {
	// DataID is the DataID of the file that was transferred. This should match
	// the DataID of the ServerFileTransferBegin message.
	DataID uint32
}

func (m *FileTransferEnd) Type() MessageType {
	return MessageTypeFileTransferEnd
}

func (m *FileTransferEnd) Encode(w io.Writer) error {
	return encodeBinary(w, []any{
		m.DataID,
	})
}

func (m *FileTransferEnd) Decode(r io.Reader) error {
	return decodeBinary(r, []any{
		&m.DataID,
	})
}

// FileUpload is a message sent to the server to indicate that the client wants
// to upload a file to the server.
type FileUpload struct {
	// Path is the path of the file that is being uploaded.
	Path string
	// Size is the size of the file that is being uploaded.
	Size uint64
}

func (m *FileUpload) Type() MessageType {
	return MessageTypeFileUpload
}

func (m *FileUpload) Encode(w io.Writer) error {
	return encodeBinary(w, []any{
		m.Path,
		m.Size,
	})
}

func (m *FileUpload) Decode(r io.Reader) error {
	return decodeBinary(r, []any{
		&m.Path,
		&m.Size,
	})
}

// FileUploadAgree is a message sent from the server to the client to indicate
// that the server has agreed to accept the file upload. The client must receive
// the DataID of this message and send ServerMessageFileUploadData messages
// with the same DataID.
type FileUploadAgree struct {
	// Path is the path of the file that is being uploaded.
	Path string
	// DataID is the DataID of the file that is being uploaded. The client must
	// send ServerMessageFileUploadData messages with this DataID.
	DataID uint32
	// DataSize is a hint for the size of the data that will be sent in the
	// ServerMessageFileUploadData messages. The server should use this to
	// allocate a buffer for the data.
	DataSize uint32
}

func (m *FileUploadAgree) Type() MessageType {
	return MessageTypeFileUploadAgree
}

func (m *FileUploadAgree) Encode(w io.Writer) error {
	return encodeBinary(w, []any{
		m.Path,
		m.DataID,
		m.DataSize,
	})
}

func (m *FileUploadAgree) Decode(r io.Reader) error {
	return decodeBinary(r, []any{
		&m.Path,
		&m.DataID,
		&m.DataSize,
	})
}

// FileUploadData is a message sent from the client to the server to indicate
// that the client is sending over a fragment of a file to the server. It is
// sent after a FileUploadAgree message.
type FileUploadData struct {
	// DataID is the DataID of the file that is being uploaded. This should
	// match the DataID of the FileUploadAgree message.
	DataID uint32
	// Data is the fragment of the file that is being uploaded.
	// It may be of any length. The server should join all fragments together
	// and determine if enough data has been received to reconstruct the file.
	Data []byte
}

func (m *FileUploadData) Type() MessageType {
	return MessageTypeFileUploadData
}

func (m *FileUploadData) Encode(w io.Writer) error {
	return encodeBinary(w, []any{
		m.DataID,
		m.Data,
	})
}

// Decode reads a FileUploadData message from r.
// If m.Data is not large enough to hold the data, it will be reallocated,
// otherwise it will be reused. As such, it is recommended to reuse the same
// FileUploadData instance per data ID.
func (m *FileUploadData) Decode(r io.Reader) error {
	if err := m.DecodeDataID(r); err != nil {
		return err
	}
	if err := decodeBytesBuf(r, &m.Data); err != nil {
		return err
	}
	return nil
}

// DecodeDataID reads a FileUploadData message from r, but only reads the
// DataID.
func (m *FileUploadData) DecodeDataID(r io.Reader) error {
	return binary.Read(r, Endianness, &m.DataID)
}

// DecodeData reads a FileUploadData message from r, but only reads the data.
func (m *FileUploadData) DecodeData(r io.Reader) error {
	return decodeBytesBuf(r, &m.Data)
}

// FileUploadEnd is a message sent from the client to the server to indicate
// that the client has finished uploading a file to the server. It is sent
// after a FileUploadAgree message.
type FileUploadEnd struct {
	// DataID is the DataID of the file that is being uploaded. This should
	// match the DataID of the FileUploadAgree message.
	DataID uint32
}

func (m *FileUploadEnd) Type() MessageType {
	return MessageTypeFileUploadEnd
}

func (m *FileUploadEnd) Encode(w io.Writer) error {
	return encodeBinary(w, []any{
		m.DataID,
	})
}

func (m *FileUploadEnd) Decode(r io.Reader) error {
	return decodeBinary(r, []any{
		&m.DataID,
	})
}

func encodeBinary(w io.Writer, values []any) error {
	for _, value := range values {
		switch value := value.(type) {
		case string:
			if err := binary.Write(w, Endianness, uint32(len(value))); err != nil {
				return err
			}
			if _, err := io.WriteString(w, value); err != nil {
				return err
			}
		case []byte:
			if err := binary.Write(w, Endianness, uint32(len(value))); err != nil {
				return err
			}
			if _, err := w.Write(value); err != nil {
				return err
			}
		default:
			if err := binary.Write(w, Endianness, value); err != nil {
				return err
			}
		}
	}
	return nil
}

func decodeBinary(r io.Reader, values []any) error {
	for _, value := range values {
		switch value := value.(type) {
		case *string, *[]byte:
			if err := decodeStringAny(r, value); err != nil {
				return err
			}
		default:
			if err := binary.Read(r, Endianness, value); err != nil {
				return err
			}
		}
	}
	return nil
}

func decodeString[T string | []byte](r io.Reader, dst *T) error {
	return decodeStringAny(r, dst)
}

func decodeStringAny(r io.Reader, dst any) error {
	var strlen uint32
	if err := binary.Read(r, Endianness, &strlen); err != nil {
		return err
	}

	var str []byte
	if strlen > 0 {
		str = make([]byte, strlen)
		if _, err := io.ReadFull(r, str); err != nil {
			return err
		}
	}

	switch dst := any(dst).(type) {
	case *string:
		*dst = string(str)
	case *[]byte:
		*dst = str
	}

	return nil
}

func decodeBytesBuf(r io.Reader, dst *[]byte) error {
	var size uint32
	if err := binary.Read(r, Endianness, &size); err != nil {
		return err
	}

	if cap(*dst) < int(size) {
		*dst = make([]byte, size)
	} else {
		*dst = (*dst)[:size]
	}

	if _, err := io.ReadFull(r, *dst); err != nil {
		return err
	}

	return nil
}
