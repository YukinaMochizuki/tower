package tunnel

// SetWindows sets the windows the hub's end of each tunnel receives with,
// as the tests scale them down.
func (h *Hub) SetWindows(stream, connection int) {
	h.streamWindow, h.connWindow = stream, connection
}
