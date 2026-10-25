// Command tower-apiserver serves the tower API on two listeners: a TCP
// address for clients, who authenticate with a service token's secret, and
// a unix socket for root on the host, where the first admin token is made.
// See docs/decisions/authentication.md.
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/secrets"
	"github.com/YukinaMochizuki/tower/internal/store/sqlite"
	"github.com/YukinaMochizuki/tower/internal/ticket"
)

type flags struct {
	db           string
	listen       string
	adminSocket  string
	secretsKEK   string
	previousKEKs []string
	ticketKey    string
	defaultImage string
	dev          bool
}

func main() {
	var f flags
	flag.StringVar(&f.db, "db", "tower.db", "path of the SQLite database")
	flag.StringVar(&f.listen, "listen", "127.0.0.1:8080", "address for the public API")
	flag.StringVar(&f.adminSocket, "admin-socket", "", "unix socket for root on this host, who may do what an admin token may and issue tokens; empty disables it")
	flag.StringVar(&f.secretsKEK, "secrets-kek", "", "the key-encryption key Secrets are sealed under, a file only its owner reads; missing, Secrets cannot be written, and the apiserver will not start while any are stored")
	flag.Func("secrets-kek-previous", "a previous key-encryption key, only to open what it sealed, which is sealed again under --secrets-kek at start; repeatable", func(s string) error {
		f.previousKEKs = append(f.previousKEKs, s)
		return nil
	})
	flag.StringVar(&f.ticketKey, "ticket-key", "", "the ed25519 key tickets for direct streams are signed with, a file only its owner reads, made if missing; its public key, which hostd verifies tickets with, is written beside it with .pub added. Empty is ticket-key beside --secrets-kek; with neither, no ticket is issued")
	flag.StringVar(&f.defaultImage, "default-box-image", "", "the image a box of a repository without a devcontainer is made of when it names no fallback_image, as workstream's own default box image; empty fails such a box")
	flag.BoolVar(&f.dev, "dev", false, "for local development only: trust the "+auth.DevPrincipalHeader+" header as the caller, and take the admin socket from the uid the apiserver runs as; only on a loopback IP")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, f, log); err != nil {
		log.Error("apiserver stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, f flags, log *slog.Logger) error {
	if err := checkDev(f); err != nil {
		return err
	}
	host, err := os.Hostname()
	if err != nil {
		return err
	}
	st, err := sqlite.Open(ctx, f.db)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = st.Close() }()

	var admin net.Listener
	if f.adminSocket != "" {
		if admin, err = listenUnix(f.adminSocket, 0o600); err != nil {
			return fmt.Errorf("--admin-socket: %w", err)
		}
		defer func() { _ = admin.Close() }()
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", f.listen)
	if err != nil {
		return err
	}
	keys, err := loadKeyring(f.secretsKEK, f.previousKEKs, log)
	if err != nil {
		return err
	}
	tickets, err := loadTicketKey(f, log)
	if err != nil {
		return err
	}
	srv := apiserver.New(apiserver.Config{Store: st, Logger: log, Secrets: keys, Credentials: st, Tickets: tickets, DefaultBoxImage: f.defaultImage})
	tokens, ok := srv.ServiceTokens()
	if !ok {
		return errors.New("the apiserver keeps no credentials")
	}
	public := auth.Chain{tokens}
	uid := adminUID
	if f.dev {
		public = append(public, auth.Dev{})
		uid = uint32(os.Getuid())
	}
	// A key-encryption key that does not open what is stored stops the
	// apiserver here, before it serves anything.
	if err := srv.PrepareSecrets(ctx); err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return srv.Run(ctx) })
	g.Go(func() error {
		log.Info("serving the public API", "address", ln.Addr().String(), "dev", f.dev)
		return serve(ctx, &http.Server{Handler: srv.Handler(public), Protocols: protocols()}, ln)
	})
	if admin != nil {
		// Nothing but LocalRoot, which reads only AdminConnContext's record:
		// no header can say who is calling here.
		root := auth.Chain{auth.LocalRoot{Host: host, UID: uid}}
		g.Go(func() error {
			log.Info("serving the admin socket", "path", f.adminSocket, "uid", uid)
			h := auditAdmin(log, root, srv.Handler(root))
			return serve(ctx, &http.Server{Handler: h, Protocols: protocols(), ConnContext: auth.AdminConnContext}, admin)
		})
	}
	return g.Wait()
}

// adminUID is the uid the admin socket accepts: root, except in tests and
// with --dev.
var adminUID uint32

// checkDev refuses --dev anywhere but a loopback IP (127.0.0.0/8 or ::1).
func checkDev(f flags) error {
	if f.dev && !loopback(f.listen) {
		return fmt.Errorf("--dev trusts a plain header, so it only runs on a loopback IP such as 127.0.0.1:8080 or [::1]:8080, not %q", f.listen)
	}
	return nil
}

// loopback holds when addr's host is a loopback IP. Not localhost, which a
// hosts file can point anywhere.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// listenUnix listens on a unix socket at path, of mode, replacing a socket
// an earlier run left there; anything else at path is refused.
func listenUnix(path string, mode os.FileMode) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().Type() != fs.ModeSocket {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	// Made with no access for anyone, then opened to mode, so no one
	// connects in between.
	old := syscall.Umask(0o777)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, mode); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// auditAdmin logs every request on the admin socket, accepted or not, with
// the process that made it: what root does there is an admin's.
func auditAdmin(log *slog.Logger, chain auth.Chain, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attrs := []any{"method", r.Method, "path", r.URL.Path}
		if cred, ok := auth.AdminPeer(r.Context()); ok {
			attrs = append(attrs, "uid", cred.UID, "pid", cred.PID)
		}
		if ctx, err := chain.Authenticate(r.Context(), r.Header); err != nil {
			log.Warn("admin socket request refused", append(attrs, "error", err.Error())...)
		} else {
			p, _ := auth.FromContext(ctx)
			log.Info("admin socket request", append(attrs, "principal", p.String())...)
		}
		next.ServeHTTP(w, r)
	})
}

// protocols serves HTTP/1.1 and cleartext HTTP/2 on the same port, so
// streaming works whichever a client speaks.
func protocols() *http.Protocols {
	p := &http.Protocols{}
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return p
}

func serve(ctx context.Context, srv *http.Server, ln net.Listener) error {
	// Requests run under ctx, so stopping ends open watches and Shutdown
	// does not wait for them.
	srv.BaseContext = func(net.Listener) context.Context { return ctx }
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		_ = srv.Close()
	}
	return nil
}

// loadTicketKey loads the key tickets are signed with, from --ticket-key or
// beside the KEK, making it if missing, and writes its public key beside
// it. Without either flag it is nil, and no ticket is issued.
func loadTicketKey(f flags, log *slog.Logger) (*ticket.Signer, error) {
	path := f.ticketKey
	if path == "" && f.secretsKEK != "" {
		path = filepath.Join(filepath.Dir(f.secretsKEK), "ticket-key")
	}
	if path == "" {
		log.Warn("no --ticket-key nor --secrets-kek: no ticket for a direct stream is issued")
		return nil, nil
	}
	key, err := ticket.LoadOrCreateKey(path)
	if err != nil {
		return nil, fmt.Errorf("--ticket-key: %w", err)
	}
	s := ticket.NewSigner(key)
	pub := base64.StdEncoding.EncodeToString(s.PublicKey()) + "\n"
	if err := os.WriteFile(path+".pub", []byte(pub), 0o644); err != nil { //nolint:gosec // a public key
		return nil, fmt.Errorf("writing the ticket key's public key: %w", err)
	}
	log.Info("loaded the ticket signing key", "ticket_key", path, "public_key", strings.TrimSpace(pub))
	return s, nil
}

// loadKeyring loads the key-encryption keys. Without a current one, it is
// nil: Secrets cannot be written, and PrepareSecrets refuses to start over
// stored ones. A previous key's file may be missing, as it is but while a
// key is rotated, and is then skipped. A key file that is there but
// unusable fails, and so does a previous key without a current one.
func loadKeyring(current string, previous []string, log *slog.Logger) (*secrets.Keyring, error) {
	if current == "" {
		if len(previous) > 0 {
			return nil, errors.New("--secrets-kek-previous needs --secrets-kek")
		}
		log.Warn("no --secrets-kek: Secrets cannot be written")
		return nil, nil
	}
	kek, err := secrets.LoadKEK(current)
	if errors.Is(err, fs.ErrNotExist) {
		for _, p := range previous {
			if _, err := os.Stat(p); err == nil {
				return nil, fmt.Errorf("--secrets-kek-previous %s is there, and --secrets-kek %s is not: a rotation needs the new key too", p, current)
			}
		}
		log.Warn("the key-encryption key file is missing: Secrets cannot be written", "secrets_kek", current)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("--secrets-kek: %w", err)
	}
	var olds []secrets.KEK
	for _, p := range previous {
		k, err := secrets.LoadKEK(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("--secrets-kek-previous: %w", err)
		}
		olds = append(olds, k)
	}
	keys, err := secrets.NewKeyring(kek, olds...)
	if err != nil {
		return nil, err
	}
	log.Info("loaded the key-encryption key", "secrets_kek", current, "id", kek.ID, "previous", len(olds))
	return keys, nil
}
