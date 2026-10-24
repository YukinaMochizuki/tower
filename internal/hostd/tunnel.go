package hostd

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"

	hostdprovider "github.com/YukinaMochizuki/tower/internal/provider/hostd"
	"github.com/YukinaMochizuki/tower/internal/tunnel"
)

// Handler serves HostService over the host, as tower calls it on the
// tunnel.
func (h *Host) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle(hostdprovider.NewHandler(h))
	return mux
}

// Agent is the host's end of its tunnel to the tower at towerURL (http or
// https), opened with the ROLE_HOSTD token's secret and serving handler.
// Its Run keeps the tunnel open, dialling again as hangar's node agent
// does (tunnel.Agent).
func Agent(towerURL, secret string, handler http.Handler, log *slog.Logger) (*tunnel.Agent, error) {
	u, err := url.Parse(towerURL)
	if err != nil {
		return nil, fmt.Errorf("tower's URL: %w", err)
	}
	host := u.Host
	var dial func(ctx context.Context) (net.Conn, error)
	switch u.Scheme {
	case "http":
		if u.Port() == "" {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
		dial = func(ctx context.Context) (net.Conn, error) { return (&net.Dialer{}).DialContext(ctx, "tcp", host) }
	case "https":
		if u.Port() == "" {
			host = net.JoinHostPort(u.Hostname(), "443")
		}
		// HTTP/1.1 alone: the tunnel opens by an upgrade.
		d := &tls.Dialer{Config: &tls.Config{ServerName: u.Hostname(), NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12}}
		dial = func(ctx context.Context) (net.Conn, error) { return d.DialContext(ctx, "tcp", host) }
	default:
		return nil, fmt.Errorf("tower's URL %q is neither http nor https", towerURL)
	}
	return &tunnel.Agent{
		Dial: dial, Header: http.Header{"Authorization": {"Bearer " + secret}}, Handler: handler, Log: log,
	}, nil
}
