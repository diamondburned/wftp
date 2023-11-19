package wftp

// FrontendHandler is an interface for objects that can handle events for the
// frontend.
type FrontendHandler interface {
	// HandleEvent handles an event for the frontend.
	HandleEvent(FrontendEvent)
}

// FrontendEvent is an interface for events that can be sent to the frontend.
// It is meant solely for display purposes.
type FrontendEvent interface {
	frontendEvent()
}

// PeerConnectedEvent is an event that is sent to the frontend when a peer
// connects.
type PeerConnectedEvent struct {
	IDs []string
}

// PeerDisconnectedEvent is an event that is sent to the frontend when a peer
// disconnects.
type PeerDisconnectedEvent struct {
	IDs []string
}

// FileEvent is an event that is sent to the frontend when a file is
// uploaded.
type FileEvent struct {
	PeerID string
	Path   string
	Size   uint64
	Upload bool // true if the file was uploaded, false if it was downloaded
}

// FileProgressEvent is an event that is sent to the frontend when a file
// progresses. There is no guarantee that the frontend will receive all progress
// events.
type FileProgressEvent struct {
	PeerID  string
	Path    string
	Written uint64
}

func (PeerConnectedEvent) frontendEvent()    {}
func (PeerDisconnectedEvent) frontendEvent() {}
func (FileEvent) frontendEvent()             {}
func (FileProgressEvent) frontendEvent()     {}
