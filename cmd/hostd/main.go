// Command hostd runs a hostd target's host: it dials tower and opens the
// tunnel tower calls it on, follows the target's Instances and Goldens
// through the API with its ROLE_HOSTD token, makes them real through the
// host's drivers, and serves direct streams on tickets tower signs. It
// keeps no state of its own and decides nothing (internal/hostd). Its
// drivers are the host's; a build with the hostdfake tag, which a release
// never has, runs on a fake host instead, for tests and the demo.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/hostd"
	"github.com/YukinaMochizuki/tower/internal/ticket"
)

type flags struct {
	tower, tokenFile, ticketKey, stateDir string
	target                                string
	directListen, directURL               string
	reportEvery, sweepEvery, sweepGrace   time.Duration
}

func main() {
	var f flags
	flag.StringVar(&f.tower, "tower", "", "tower's URL, such as https://tower.tailnet:8080")
	flag.StringVar(&f.target, "target", "", "the Target this host is, the one its token is for")
	flag.StringVar(&f.tokenFile, "token-file", "", "file holding the ROLE_HOSTD token's secret, only its owner may read it")
	flag.StringVar(&f.ticketKey, "ticket-public-key", "", "tower's public key for tickets, the .pub file beside its --ticket-key; empty refuses every direct stream")
	flag.StringVar(&f.stateDir, "state-dir", "/var/lib/hostd", "directory the drivers work in")
	flag.StringVar(&f.directListen, "direct-listen", "", "address direct streams are served on, the host's tailnet address and a port, such as 100.64.0.7:7443; required, so it is never every interface by default")
	flag.StringVar(&f.directURL, "direct-url", "", "where clients reach --direct-listen, such as http://host.tailnet:7443; empty is the address listened on, this host's name for every interface's")
	flag.DurationVar(&f.reportEvery, "report-every", hostd.DefaultReportEvery, "how often the target is reported and every instance read back")
	flag.DurationVar(&f.sweepEvery, "sweep-every", hostd.DefaultSweepEvery, "how often what nothing accounts for is swept")
	flag.DurationVar(&f.sweepGrace, "sweep-grace", hostd.DefaultSweepGrace, "how old what nothing accounts for must be to be swept")
	driverFlags()
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, f, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("hostd stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, f flags, log *slog.Logger) error {
	if f.tower == "" || f.target == "" || f.tokenFile == "" || f.directListen == "" {
		return errors.New("--tower, --target, --token-file and --direct-listen are required")
	}
	secret, err := readSecret(f.tokenFile)
	if err != nil {
		return fmt.Errorf("--token-file: %w", err)
	}
	d, err := drivers(f, log)
	if err != nil {
		return err
	}
	cfg := hostd.Config{
		Target: f.target, Drivers: d, Client: client.New(bearer(secret), f.tower),
		ReportEvery: f.reportEvery, SweepEvery: f.sweepEvery, SweepGrace: f.sweepGrace, Log: log,
	}
	if f.ticketKey != "" {
		if cfg.TicketKey, err = ticket.LoadPublicKey(f.ticketKey); err != nil {
			return fmt.Errorf("--ticket-public-key: %w", err)
		}
	}
	direct, err := (&net.ListenConfig{}).Listen(ctx, "tcp", f.directListen)
	if err != nil {
		return fmt.Errorf("--direct-listen: %w", err)
	}
	defer func() { _ = direct.Close() }()
	if cfg.StreamURL, err = streamURL(f.directURL, direct.Addr()); err != nil {
		return err
	}
	h := hostd.New(cfg)
	agent, err := hostd.Agent(f.tower, secret, h.Handler(), log)
	if err != nil {
		return fmt.Errorf("--tower: %w", err)
	}
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return h.Run(ctx) })
	g.Go(func() error {
		log.Info("opening the tunnel to tower", "tower", f.tower, "target", f.target)
		agent.Run(ctx)
		return nil
	})
	srv := &http.Server{Handler: h.Streams(), ReadHeaderTimeout: 10 * time.Second}
	g.Go(func() error {
		log.Info("serving direct streams", "address", direct.Addr().String(), "url", cfg.StreamURL)
		if err := srv.Serve(direct); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done()
		// Open streams end with hostd; their clients dial again.
		return srv.Close()
	})
	return g.Wait()
}

// streamURL is where clients reach direct streams: the flag, or the
// address listened on, with this host's name for an address of every
// interface.
func streamURL(flagURL string, addr net.Addr) (string, error) {
	if flagURL != "" {
		return strings.TrimSuffix(flagURL, "/"), nil
	}
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "", err
	}
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		if host, err = os.Hostname(); err != nil {
			return "", err
		}
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

// readSecret reads the token's secret from a file only its owner may read.
func readSecret(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is mode %#o: a token's file is its owner's alone (0600)", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	secret := strings.TrimSpace(string(b))
	if secret == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return secret, nil
}

func bearer(secret string) *http.Client {
	return &http.Client{Transport: header{secret: secret}}
}

type header struct{ secret string }

func (h header) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+h.secret)
	return http.DefaultTransport.RoundTrip(r)
}
