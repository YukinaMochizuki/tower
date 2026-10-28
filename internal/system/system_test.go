package system_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/system"
)

// local runs tower-system's calls in the test's process, over a directory
// of its own, as the system container runs them.
type local struct{ cfg system.Config }

func (l local) RunSystem(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	return system.Serve(ctx, l.cfg, stdin, stdout, stderr), nil
}

// remote makes a bare repository with two commits on main.
func remote(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	bare, work := filepath.Join(dir, "remote.git"), filepath.Join(dir, "work")
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(dir, "init", "--quiet", "--bare", "--initial-branch=main", bare)
	run(dir, "init", "--quiet", "--initial-branch=main", work)
	for _, c := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(work, "README"), []byte(c+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run(work, "add", "-A")
		run(work, "commit", "--quiet", "-m", c)
	}
	run(work, "push", "--quiet", bare, "main")
	return bare, run(work, "rev-parse", "HEAD")
}

func names(t *testing.T, b []byte) []string {
	t.Helper()
	var out []string
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, h.Name)
	}
}

// TestTheMirrorsThroughCalls: a fetch, a checkout and its tar, an origin,
// a count behind and a read go through the calls as the git driver does
// them, and what is not there says so.
func TestTheMirrorsThroughCalls(t *testing.T) {
	o := system.Ops{R: local{cfg: system.Config{Root: t.TempDir()}}}
	ctx := t.Context()
	r, head := remote(t)
	got, err := o.Fetch(ctx, driver.FetchSpec{Mirror: "web/api", URL: r, Branch: "main", Token: "secret-token-1"})
	if err != nil || got != head {
		t.Fatalf("fetched %q, %v; want %q", got, err, head)
	}
	var checkout bytes.Buffer
	commit, err := o.Checkout(ctx, "web/api", "", &checkout)
	if err != nil || commit != head {
		t.Fatalf("checked out %q, %v", commit, err)
	}
	if n := names(t, checkout.Bytes()); !strings.Contains(strings.Join(n, " "), "README") || bytes.Contains(checkout.Bytes(), []byte("secret-token-1")) {
		t.Errorf("the checkout's tar holds %v", n)
	}
	var origin bytes.Buffer
	if err := o.Origin(ctx, "web/api", &origin); err != nil || len(names(t, origin.Bytes())) == 0 {
		t.Errorf("the origin: %d bytes, %v", origin.Len(), err)
	}
	if n, err := o.CommitsBehind(ctx, "web/api", head, "main"); err != nil || n != 0 {
		t.Errorf("behind %d, %v", n, err)
	}
	c, entries, err := o.Read(ctx, "web/api", "", []string{"README", "none"}, 1<<10)
	if err != nil || c != head || string(entries["README"].Content) != "two\n" {
		t.Errorf("read %q %+v, %v", c, entries, err)
	}
	if _, err := o.Checkout(ctx, "web/never", "", io.Discard); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("a mirror never fetched: %v, want ErrNotFound", err)
	}
	if _, err := o.Fetch(ctx, driver.FetchSpec{Mirror: "web/api", URL: r, Branch: "none"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("a branch not there: %v, want ErrNotFound", err)
	}
}

// TestFetchesAtOnceKeepTheMirrorWhole: fetches of a mirror at once, each
// a call of its own as in the system container, all succeed, and the
// mirror is whole after: each call holds the mirror apart from the others.
func TestFetchesAtOnceKeepTheMirrorWhole(t *testing.T) {
	r, head := remote(t)
	for round := range 10 {
		cfg := system.Config{Root: t.TempDir()}
		errs := make(chan error, 8)
		for range 8 {
			// A Driver of its own each, as a call's process has.
			o := system.Ops{R: local{cfg: cfg}}
			go func() {
				got, err := o.Fetch(t.Context(), driver.FetchSpec{Mirror: "web/api", URL: r, Branch: "main"})
				if err == nil && got != head {
					err = errors.New("fetched " + got)
				}
				errs <- err
			}()
		}
		for range 8 {
			if err := <-errs; err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
		if _, err := (system.Ops{R: local{cfg: cfg}}).Checkout(t.Context(), "web/api", "", io.Discard); err != nil {
			t.Fatalf("round %d: the mirror after: %v", round, err)
		}
	}
}

// TestABuildNeedsItsBuilder: a build with no buildkitd to reach fails,
// saying so, rather than hanging or answering nothing.
func TestABuildNeedsItsBuilder(t *testing.T) {
	root := t.TempDir()
	o := system.Ops{R: local{cfg: system.Config{Root: root, Registry: "registry.example", BuildKit: "unix://" + filepath.Join(root, "none.sock")}}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	_ = tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: 13})
	_, _ = tw.Write([]byte("FROM scratch\n"))
	_ = tw.Close()
	if _, err := o.Build(ctx, driver.BuildSpec{Repository: "web/x", Context: &b}, "", "", system.Login{}); err == nil {
		t.Error("a build with no buildkitd succeeded")
	}
}

// TestTheStatusAnswersWithoutDocker: the container has no Docker, and its
// status answers, the builder made without it, saying only that
// buildkitd does not answer when it does not.
func TestTheStatusAnswersWithoutDocker(t *testing.T) {
	root := t.TempDir()
	o := system.Ops{R: local{cfg: system.Config{Root: root, Registry: "registry.example", RegistryPlainHTTP: true, BuildKit: "unix://" + filepath.Join(root, "none.sock")}}}
	st, err := o.Status(t.Context())
	if err != nil {
		t.Fatalf("the status: %v", err)
	}
	if st.BuildKit != "" {
		t.Errorf("buildkitd said to answer as %q, with none there", st.BuildKit)
	}
}

// answering is a tower-system that answers every call with one line.
type answering string

func (a answering) RunSystem(_ context.Context, _ io.Reader, stdout, _ io.Writer) (int, error) {
	_, err := io.WriteString(stdout, string(a)+"\n")
	return 0, err
}

// TestAResolveCarriesTheImagesEnvironment: what a resolve answers of an
// image reaches its caller whole, its environment with its digest and
// user.
func TestAResolveCarriesTheImagesEnvironment(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	o := system.Ops{R: answering(`{"digest":"sha256:01","user":"1000","env":["PATH=/bin","GOCACHE=/cache/go"]}`)}
	cfg, err := o.Resolve(ctx, "registry.example/app:1", system.Login{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Digest != "sha256:01" || cfg.User != "1000" || !slices.Equal(cfg.Env, []string{"PATH=/bin", "GOCACHE=/cache/go"}) {
		t.Errorf("resolved %+v", cfg)
	}
}
