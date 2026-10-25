package hostd

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"path"
	"strconv"
	"strings"

	"connectrpc.com/connect"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/provider"
)

// The imperative half: what tower asks of the host over its tunnel. An
// instance is found by reading its Instance, whose uid names what serves
// it on the host.

// placed is an instance as the host serves it.
type placed struct {
	ref  provider.Instance
	in   *envv1alpha1.Instance
	g    driver.Guests
	name string
}

// find reads the instance's Instance. One not there, or another target's,
// is ErrNotFound; an API that does not answer, ErrUnavailable.
func (h *Host) find(ctx context.Context, ref provider.Instance) (placed, error) {
	o, err := h.c.Get(ctx, registry.Instance, client.Key{Project: ref.Project, Name: ref.Name})
	if connect.CodeOf(err) == connect.CodeNotFound {
		return placed{}, fmt.Errorf("instance %s/%s: %w", ref.Project, ref.Name, provider.ErrNotFound)
	}
	if err != nil {
		return placed{}, fmt.Errorf("reading instance %s/%s: %w: %w", ref.Project, ref.Name, err, provider.ErrUnavailable)
	}
	in := o.(*envv1alpha1.Instance)
	if in.GetSpec().GetTarget() != h.cfg.Target {
		return placed{}, fmt.Errorf("instance %s/%s is target %q's: %w", ref.Project, ref.Name, in.GetSpec().GetTarget(), provider.ErrNotFound)
	}
	return placed{ref: ref, in: in, g: h.guests(in), name: guestName(in.GetMetadata().GetUid())}, nil
}

// guests is the driver of the instance's kind.
func (h *Host) guests(in *envv1alpha1.Instance) driver.Guests {
	if in.GetSpec().GetKind() == envv1alpha1.InstanceKind_INSTANCE_KIND_VM {
		return h.d.VMs
	}
	return h.d.Containers
}

// running finds an instance whose guest runs.
func (h *Host) running(ctx context.Context, ref provider.Instance) (placed, error) {
	p, err := h.find(ctx, ref)
	if err != nil {
		return p, err
	}
	g, err := p.g.Inspect(ctx, p.name)
	if err != nil {
		return p, fmt.Errorf("instance %s/%s: %w", ref.Project, ref.Name, providerError(err))
	}
	if !g.Running {
		return p, fmt.Errorf("instance %s/%s is not running: %w", ref.Project, ref.Name, provider.ErrFailed)
	}
	return p, nil
}

func (h *Host) Exec(ctx context.Context, req provider.ExecRequest) (int, error) {
	p, err := h.running(ctx, req.Instance)
	if err != nil {
		return 0, err
	}
	code, err := p.g.Exec(ctx, p.name, driver.ExecSpec{
		Command: req.Command, Env: req.Env, Dir: req.Dir, Stdin: req.Stdin, Stdout: req.Stdout, Stderr: req.Stderr,
	})
	return code, providerError(err)
}

func (h *Host) ReadFile(ctx context.Context, in provider.Instance, p string, w io.Writer) error {
	pl, err := h.running(ctx, in)
	if err != nil {
		return err
	}
	return providerError(pl.g.ReadFile(ctx, pl.name, p, w))
}

func (h *Host) WriteFile(ctx context.Context, in provider.Instance, p string, mode fs.FileMode, offset int64, r io.Reader) error {
	pl, err := h.running(ctx, in)
	if err != nil {
		return err
	}
	return providerError(pl.g.WriteFile(ctx, pl.name, p, mode, offset, r))
}

// Copy streams a tar of fromPath out of one guest and into the other at
// toPath: CopyOut names it by its base name, which becomes toPath's.
func (h *Host) Copy(ctx context.Context, from provider.Instance, fromPath string, to provider.Instance, toPath string) error {
	src, err := h.running(ctx, from)
	if err != nil {
		return err
	}
	dst, err := h.running(ctx, to)
	if err != nil {
		return err
	}
	fromPath, toPath = path.Clean("/"+fromPath), path.Clean("/"+toPath)
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(src.g.CopyOut(ctx, src.name, fromPath, pw))
	}()
	renamed := rename(pr, path.Base(fromPath), path.Base(toPath))
	err = dst.g.CopyIn(ctx, dst.name, path.Dir(toPath), renamed)
	_ = pr.CloseWithError(errors.New("copy ended"))
	return providerError(err)
}

// rename is the tar r with its top name from changed to to.
func rename(r io.Reader, from, to string) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		tr, tw := tar.NewReader(r), tar.NewWriter(pw)
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				pw.CloseWithError(tw.Close())
				return
			}
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			if rest, ok := strings.CutPrefix(hdr.Name, from); ok && (rest == "" || rest[0] == '/') {
				hdr.Name = to + rest
			}
			if err := tw.WriteHeader(hdr); err != nil {
				pw.CloseWithError(err)
				return
			}
			if _, err := io.Copy(tw, tr); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
	}()
	return pr
}

// BuildImage builds a context tower sent, or a mirror's work tree, which
// goes from the git driver to the image driver on the host alone.
func (h *Host) BuildImage(ctx context.Context, req provider.BuildRequest) (string, error) {
	spec := driver.BuildSpec{Repository: req.Repository, Context: req.Context, Dir: req.Dir, Dockerfile: req.Dockerfile}
	var archived chan error
	var pr *io.PipeReader
	if m := req.Mirror; m != nil {
		var pw *io.PipeWriter
		pr, pw = io.Pipe()
		archived = make(chan error, 1)
		go func() {
			_, err := h.d.Git.Archive(ctx, m.Mirror, m.Ref, pw)
			pw.CloseWithError(err)
			archived <- err
		}()
		spec.Context = pr
	}
	ref, err := h.d.Images.Build(ctx, spec)
	if archived != nil {
		// A build that ended before reading it all ends the archive too.
		_ = pr.CloseWithError(errors.New("build ended"))
		// An archive that failed failed the build with it, as its tar
		// ended short; a mirror or ref not there is said as such.
		if aerr := <-archived; err != nil && errors.Is(aerr, driver.ErrNotFound) {
			return "", providerError(aerr)
		}
	}
	if err != nil && !errors.Is(err, driver.ErrNoSpace) && !errors.Is(err, context.Canceled) {
		return "", fmt.Errorf("building %s: %w: %w", req.Repository, err, provider.ErrFailed)
	}
	return ref, providerError(err)
}

func (h *Host) ResolveImage(ctx context.Context, ref string) (provider.ImageConfig, error) {
	cfg, err := h.d.Images.Resolve(ctx, ref)
	return provider.ImageConfig{Digest: cfg.Digest, User: cfg.User}, providerError(err)
}

func (h *Host) ReadMirror(ctx context.Context, req provider.MirrorReadRequest) (provider.MirrorRead, error) {
	var most int64
	if req.Content {
		most = provider.MaxMirrorContent
	}
	commit, entries, err := h.d.Git.Read(ctx, req.Mirror, req.Ref, req.Paths, most)
	if err != nil {
		return provider.MirrorRead{}, providerError(err)
	}
	out := provider.MirrorRead{Commit: commit, Entries: map[string]provider.MirrorEntry{}}
	for p, e := range entries {
		out.Entries[p] = provider.MirrorEntry{Object: e.Object, Dir: e.Dir, Size: e.Size, Content: e.Content}
	}
	return out, nil
}

func (h *Host) Capture(ctx context.Context, in provider.Instance) (provider.Artifact, error) {
	p, err := h.find(ctx, in)
	if err != nil {
		return provider.Artifact{}, err
	}
	uid := p.in.GetMetadata().GetUid()
	g, err := p.g.Inspect(ctx, p.name)
	if err != nil {
		return provider.Artifact{}, fmt.Errorf("instance %s/%s: %w", in.Project, in.Name, providerError(err))
	}
	if g.Running {
		return provider.Artifact{}, fmt.Errorf("instance %s/%s is running; capture takes a stopped one: %w", in.Project, in.Name, provider.ErrFailed)
	}
	golden, err := h.d.Storage.Capture(ctx, diskName(uid), goldenName(uid), driver.Labels{
		driver.LabelOwner: h.cfg.Target, driver.LabelUID: uid, driver.LabelProject: in.Project, driver.LabelInstance: in.Name,
	})
	if err != nil {
		return provider.Artifact{}, providerError(err)
	}
	return provider.Artifact{Name: golden.Name, Size: golden.Size}, nil
}

// DeleteArtifact destroys a golden this hostd made; the disks cloned from
// it keep what it held, and it goes with the last of them.
func (h *Host) DeleteArtifact(ctx context.Context, artifact string) error {
	goldens, err := h.d.Storage.Goldens(ctx)
	if err != nil {
		return providerError(err)
	}
	for _, g := range goldens {
		if g.Name != artifact {
			continue
		}
		if !h.mine(g.Labels) {
			return fmt.Errorf("artifact %s is not this hostd's to delete: %w", artifact, provider.ErrFailed)
		}
		return providerError(h.d.Storage.DestroyGolden(ctx, artifact))
	}
	return nil
}

func (h *Host) Fetch(ctx context.Context, req provider.FetchRequest) (string, error) {
	head, err := h.d.Git.Fetch(ctx, driver.FetchSpec{Mirror: req.Mirror, URL: req.URL, Token: req.Token, Branch: req.Branch})
	return head, providerError(err)
}

func (h *Host) Checkout(ctx context.Context, in provider.Instance, mirror, ref, dir string) (string, error) {
	p, err := h.running(ctx, in)
	if err != nil {
		return "", err
	}
	pr, pw := io.Pipe()
	type result struct {
		commit string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		commit, err := h.d.Git.Checkout(ctx, mirror, ref, pw)
		pw.CloseWithError(err)
		done <- result{commit, err}
	}()
	cerr := p.g.CopyIn(ctx, p.name, path.Clean("/"+dir), pr)
	_ = pr.CloseWithError(errors.New("checkout ended"))
	res := <-done
	if res.err != nil {
		return "", providerError(res.err)
	}
	return res.commit, providerError(cerr)
}

func (h *Host) UpdateOrigin(ctx context.Context, in provider.Instance, mirror, dir string) error {
	p, err := h.running(ctx, in)
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := h.d.Git.Origin(ctx, mirror, pw)
		pw.CloseWithError(err)
		done <- err
	}()
	cerr := p.g.CopyIn(ctx, p.name, path.Join(path.Clean("/"+dir), ".git"), pr)
	_ = pr.CloseWithError(errors.New("update ended"))
	if err := <-done; err != nil {
		return providerError(err)
	}
	return providerError(cerr)
}

func (h *Host) CommitsBehind(ctx context.Context, mirror, commit, branch string) (int, error) {
	n, err := h.d.Git.CommitsBehind(ctx, mirror, commit, branch)
	return n, providerError(err)
}

// Push takes the checkout's .git out of the instance, never its token in.
func (h *Host) Push(ctx context.Context, req provider.PushRequest) error {
	p, err := h.running(ctx, req.Instance)
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(p.g.CopyOut(ctx, p.name, path.Join(path.Clean("/"+req.Dir), ".git"), pw))
	}()
	err = h.d.Git.Push(ctx, driver.PushSpec{Mirror: req.Mirror, URL: req.URL, Token: req.Token, Branch: req.Branch, Lease: req.Lease}, pr)
	_ = pr.CloseWithError(errors.New("push ended"))
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s is no checkout: %w", req.Dir, provider.ErrFailed)
	}
	return providerError(err)
}

// Ticket is the URL of the instance's port on Streams; the ticket's own
// expiry bounds it.
func (h *Host) Ticket(ctx context.Context, in provider.Instance, port int32) (provider.Ticket, error) {
	if h.cfg.StreamURL == "" {
		return provider.Ticket{}, fmt.Errorf("this hostd serves no direct streams: %w", provider.ErrFailed)
	}
	if _, err := h.running(ctx, in); err != nil {
		return provider.Ticket{}, err
	}
	u := strings.TrimSuffix(h.cfg.StreamURL, "/") + StreamPath + url.PathEscape(in.Project) + "/" + url.PathEscape(in.Name) + "/" + strconv.Itoa(int(port))
	return provider.Ticket{URL: u}, nil
}
