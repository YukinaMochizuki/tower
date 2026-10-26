package hangar

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/streams"
)

// Direct streams to a hangar target's instances are served by tower, which
// relays each to hangar's port forward with the project's token: hangar's
// tokens are project-wide and long-lived, so no client is given one
// (docs/decisions/traffic.md). A stream's ticket is tower's, as hostd's
// is.

// newStreams is the target's stream server.
func (t *Target) newStreams() *streams.Server {
	return &streams.Server{
		Target: t.cfg.Target, Key: t.cfg.TicketKey, Prefix: t.cfg.StreamPrefix, Clock: t.clock, Log: t.log,
		Changed: t.changed,
		Dial: func(ctx context.Context, ref provider.Instance, port int32) (net.Conn, error) {
			p, _, err := t.machineRunning(ctx, ref)
			if err != nil {
				return nil, err
			}
			conn, err := t.api.PortForward(ctx, PluralContainers, p.container, port)
			if err != nil {
				return nil, providerError(err)
			}
			return conn, nil
		},
	}
}

// Streams serves direct streams to the target's instances, under
// Config.StreamPrefix.
func (t *Target) Streams() http.Handler { return t.streams }

// Ticket is the URL of the instance's port on Streams; the ticket's own
// expiry bounds it.
func (t *Target) Ticket(ctx context.Context, in provider.Instance, port int32) (provider.Ticket, error) {
	if t.cfg.StreamURL == "" {
		return provider.Ticket{}, fmt.Errorf("this tower relays no direct streams to target %s: %w", t.cfg.Target, provider.ErrFailed)
	}
	if _, _, err := t.machineRunning(ctx, in); err != nil {
		return provider.Ticket{}, err
	}
	u := strings.TrimSuffix(t.cfg.StreamURL, "/") + "/" + url.PathEscape(in.Project) + "/" + url.PathEscape(in.Name) + "/" + strconv.Itoa(int(port))
	return provider.Ticket{URL: u}, nil
}
