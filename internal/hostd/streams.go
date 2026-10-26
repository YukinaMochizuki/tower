package hostd

import (
	"context"
	"net"
	"net/http"

	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/streams"
)

// StreamPath is where the host serves direct streams
// (internal/streams): StreamPath<project>/<instance>/<port>.
const StreamPath = "/streams/"

// newStreams is the host's stream server, which dials its guests.
func (h *Host) newStreams() *streams.Server {
	return &streams.Server{
		Target: h.cfg.Target, Key: h.cfg.TicketKey, Prefix: StreamPath, Clock: h.clock, Log: h.log,
		Changed: h.changed,
		Dial: func(ctx context.Context, ref provider.Instance, port int32) (net.Conn, error) {
			p, err := h.running(ctx, ref)
			if err != nil {
				return nil, err
			}
			return p.g.Dial(ctx, p.name, port)
		},
	}
}

// Streams serves direct streams to the host's guests.
func (h *Host) Streams() http.Handler {
	mux := http.NewServeMux()
	mux.Handle(StreamPath, h.streams)
	return mux
}

// streamsOpen is how many direct streams to the instance are open.
func (h *Host) streamsOpen(ref provider.Instance) int32 { return h.streams.Open(ref) }
