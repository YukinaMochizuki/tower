package fake

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"path"
	"slices"
	"strings"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// guests is a guest driver of one kind, "container" or "vm". A
// container's files under /workspace are its disk's, the rest its own; a
// VM's are all its disk's.
type guests struct {
	h    *Host
	kind string
}

type containers struct{ guests }

type vms struct{ guests }

func (c containers) Create(_ context.Context, spec driver.ContainerSpec) error {
	return c.create("containers.Create", spec.GuestSpec, spec.Image, func() error {
		if !c.h.st.Pulled[spec.Image] {
			return fmt.Errorf("image %s is not pulled: %w", spec.Image, driver.ErrNotFound)
		}
		return nil
	})
}

func (v vms) Create(_ context.Context, spec driver.VMSpec) error {
	return v.create("vms.Create", spec.GuestSpec, spec.BaseImage, nil)
}

func (g guests) op(name string) string {
	if g.kind == "vm" {
		return "vms." + name
	}
	return "containers." + name
}

func (g guests) key(name string) string { return g.kind + "/" + name }

func (g guests) create(op string, spec driver.GuestSpec, image string, check func() error) error {
	return g.h.change(op, func() error {
		if cur := g.h.st.Guests[g.key(spec.Name)]; cur != nil {
			if cur.Image != image || cur.Spec.Disk != spec.Disk || cur.Spec.Network != spec.Network || cur.Spec.Alias != spec.Alias {
				return fmt.Errorf("%s %s is there, made otherwise: %w", g.kind, spec.Name, driver.ErrConflict)
			}
			return nil
		}
		if g.h.st.Disks[diskName(spec.Disk)] == nil {
			return fmt.Errorf("disk %s: %w", spec.Disk, driver.ErrNotFound)
		}
		if spec.Network != "" && g.h.st.Networks[g.key(spec.Network)] == nil {
			return fmt.Errorf("network %s: %w", spec.Network, driver.ErrNotFound)
		}
		if check != nil {
			if err := check(); err != nil {
				return err
			}
		}
		spec.Labels = maps.Clone(spec.Labels)
		g.h.st.Guests[g.key(spec.Name)] = &guest{Spec: spec, Image: image, Created: g.h.clock.Now(), Files: map[string][]byte{}}
		return nil
	})
}

func (g guests) Version(context.Context) (string, error) { return Version, nil }

func (g guests) EnsureNetwork(_ context.Context, name string, labels driver.Labels) error {
	return g.h.change(g.op("EnsureNetwork"), func() error {
		if g.h.st.Networks[g.key(name)] == nil {
			g.h.st.Networks[g.key(name)] = &network{Labels: maps.Clone(labels), Created: g.h.clock.Now()}
		}
		return nil
	})
}

func (g guests) Networks(context.Context) ([]driver.Network, error) {
	var out []driver.Network
	err := g.h.read(g.op("Networks"), func() error {
		for _, k := range slices.Sorted(maps.Keys(g.h.st.Networks)) {
			if kind, name, _ := cutKey(k); kind == g.kind {
				n := g.h.st.Networks[k]
				out = append(out, driver.Network{Name: name, Labels: maps.Clone(n.Labels), Created: n.Created})
			}
		}
		return nil
	})
	return out, err
}

func (g guests) RemoveNetwork(_ context.Context, name string) error {
	return g.h.change(g.op("RemoveNetwork"), func() error {
		for k, gu := range g.h.st.Guests {
			if kind, _, _ := cutKey(k); kind == g.kind && gu.Spec.Network == name {
				return fmt.Errorf("network %s has %s on it: %w", name, k, driver.ErrBusy)
			}
		}
		delete(g.h.st.Networks, g.key(name))
		return nil
	})
}

// get is a guest. The caller holds h.mu.
func (g guests) get(name string) (*guest, error) {
	gu := g.h.st.Guests[g.key(name)]
	if gu == nil {
		return nil, fmt.Errorf("%s %s: %w", g.kind, name, driver.ErrNotFound)
	}
	return gu, nil
}

// running is a running guest and its disk. The caller holds h.mu.
func (g guests) running(name string) (*guest, *disk, error) {
	gu, err := g.get(name)
	if err != nil {
		return nil, nil, err
	}
	if !gu.Running {
		return nil, nil, fmt.Errorf("%s %s: %w", g.kind, name, driver.ErrNotRunning)
	}
	d := g.h.st.Disks[diskName(gu.Spec.Disk)]
	if d == nil {
		return nil, nil, fmt.Errorf("the disk of %s %s: %w", g.kind, name, driver.ErrNotFound)
	}
	return gu, d, nil
}

func (g guests) Start(_ context.Context, name string) error {
	return g.h.change(g.op("Start"), func() error {
		gu, err := g.get(name)
		if err != nil {
			return err
		}
		if why, ok := g.h.startFails[name]; ok {
			return fmt.Errorf("%s %s: %s: %w", g.kind, name, why, driver.ErrStartFailed)
		}
		if g.h.st.Disks[diskName(gu.Spec.Disk)] == nil {
			return fmt.Errorf("%s %s: its disk is gone: %w", g.kind, name, driver.ErrStartFailed)
		}
		if !gu.Running {
			g.h.st.Addresses++
			n := g.h.st.Addresses
			gu.Running, gu.StartedAt, gu.Address = true, g.h.clock.Now(), fmt.Sprintf("10.%d.%d.%d", 64+n/62500, n/250%250, n%250+2)
		}
		return nil
	})
}

func (g guests) Stop(_ context.Context, name string) error {
	return g.h.change(g.op("Stop"), func() error {
		gu, err := g.get(name)
		if err != nil {
			return err
		}
		gu.Running, gu.Address = false, ""
		return nil
	})
}

func (g guests) Remove(_ context.Context, name string) error {
	return g.h.change(g.op("Remove"), func() error {
		delete(g.h.st.Guests, g.key(name))
		return nil
	})
}

func (g guests) guestOf(gu *guest) driver.Guest {
	spec := gu.Spec
	spec.Labels = maps.Clone(spec.Labels)
	return driver.Guest{GuestSpec: spec, Image: gu.Image, Running: gu.Running, StartedAt: gu.StartedAt, Address: gu.Address, Created: gu.Created}
}

func (g guests) Inspect(_ context.Context, name string) (driver.Guest, error) {
	var out driver.Guest
	err := g.h.read(g.op("Inspect"), func() error {
		gu, err := g.get(name)
		if err == nil {
			out = g.guestOf(gu)
		}
		return err
	})
	return out, err
}

func (g guests) List(context.Context) ([]driver.Guest, error) {
	var out []driver.Guest
	err := g.h.read(g.op("List"), func() error {
		for _, k := range slices.Sorted(maps.Keys(g.h.st.Guests)) {
			if kind, _, _ := cutKey(k); kind == g.kind {
				out = append(out, g.guestOf(g.h.st.Guests[k]))
			}
		}
		return nil
	})
	return out, err
}

// files are where a guest's path is kept, and its key there.
func (g guests) files(gu *guest, d *disk, p string) (map[string][]byte, string) {
	p = path.Clean("/" + p)
	if g.kind == "vm" {
		return d.Files, p
	}
	if rest, ok := under(p, "/workspace"); ok && rest != "" {
		return d.Files, rest
	}
	return gu.Files, p
}

// under reports whether name is root or a path below it, and the rest.
func under(name, root string) (string, bool) {
	if name == root {
		return "", true
	}
	rest, ok := strings.CutPrefix(name, strings.TrimSuffix(root, "/")+"/")
	return rest, ok
}

// walk lists the guest's files under p, by their full paths. The caller
// holds h.mu.
func (g guests) walk(gu *guest, d *disk, p string) map[string][]byte {
	p = path.Clean("/" + p)
	out := map[string][]byte{}
	add := func(files map[string][]byte, prefix string) {
		for k, b := range files {
			full := k
			if prefix != "" {
				full = path.Join(prefix, k)
			}
			if _, ok := under(full, p); ok {
				out[full] = b
			}
		}
	}
	if g.kind == "vm" {
		add(d.Files, "")
	} else {
		add(d.Files, "/workspace")
		add(gu.Files, "")
	}
	return out
}

func (g guests) Exec(ctx context.Context, name string, spec driver.ExecSpec) (int, error) {
	if len(spec.Command) > 0 && spec.Command[0] == "sleep" {
		// Runs until it is killed, as a command does when its call ends.
		err := g.h.read(g.op("Exec"), func() error {
			_, _, err := g.running(name)
			if err == nil {
				g.h.execs = append(g.h.execs, Exec{Guest: name, Command: slices.Clone(spec.Command), Env: maps.Clone(spec.Env), ExitCode: -1})
			}
			return err
		})
		if err != nil {
			return 0, err
		}
		<-ctx.Done()
		return 0, ctx.Err()
	}
	var stdin []byte
	if spec.Stdin != nil && readsStdin(spec.Command) {
		var err error
		if stdin, err = io.ReadAll(spec.Stdin); err != nil {
			return 0, err
		}
	}
	var out output
	var code int
	err := g.h.change(g.op("Exec"), func() error {
		gu, d, err := g.running(name)
		if err != nil {
			return err
		}
		if g.h.onExec != nil {
			if c, ok := g.h.onExec(name, spec); ok {
				code = c
				g.h.execs = append(g.h.execs, Exec{Guest: name, Command: slices.Clone(spec.Command), Env: maps.Clone(spec.Env), ExitCode: code})
				return nil
			}
		}
		code = shell{g: g, gu: gu, d: d, env: spec.Env, dir: spec.Dir, stdin: stdin, out: &out}.run(spec.Command)
		g.h.execs = append(g.h.execs, Exec{Guest: name, Command: slices.Clone(spec.Command), Env: maps.Clone(spec.Env), ExitCode: code})
		return nil
	})
	if err != nil {
		return 0, err
	}
	// Written once the lock is let go: a caller that reads slowly holds
	// up no other guest.
	if err := out.replay(spec.Stdout, spec.Stderr); err != nil {
		return 0, err
	}
	return code, nil
}

func (g guests) ReadFile(_ context.Context, name, p string, w io.Writer) error {
	var b []byte
	err := g.h.read(g.op("ReadFile"), func() error {
		gu, d, err := g.running(name)
		if err != nil {
			return err
		}
		files, k := g.files(gu, d, p)
		var ok bool
		if b, ok = files[k]; !ok {
			return fmt.Errorf("%s: %w", p, fs.ErrNotExist)
		}
		return nil
	})
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func (g guests) WriteFile(_ context.Context, name, p string, _ fs.FileMode, offset int64, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return g.h.change(g.op("WriteFile"), func() error {
		gu, d, err := g.running(name)
		if err != nil {
			return err
		}
		if err := g.h.room(uint64(len(b))); err != nil {
			return err
		}
		files, k := g.files(gu, d, p)
		if offset != 0 {
			if have := int64(len(files[k])); have != offset {
				return fmt.Errorf("%s holds %d bytes, not %d: %w", p, have, offset, driver.ErrBadOffset)
			}
			b = append(slices.Clone(files[k]), b...)
		}
		files[k] = b
		return nil
	})
}

func (g guests) CopyOut(_ context.Context, name, p string, w io.Writer) error {
	var files map[string][]byte
	p = path.Clean("/" + p)
	err := g.h.read(g.op("CopyOut"), func() error {
		gu, d, err := g.running(name)
		if err != nil {
			return err
		}
		files = g.walk(gu, d, p)
		if len(files) == 0 {
			return fmt.Errorf("%s: %w", p, fs.ErrNotExist)
		}
		return nil
	})
	if err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	parent := path.Dir(p)
	for _, full := range slices.Sorted(maps.Keys(files)) {
		rel, _ := under(full, parent)
		if parent == "/" {
			rel = strings.TrimPrefix(full, "/")
		}
		if err := tw.WriteHeader(&tar.Header{Name: rel, Mode: 0o644, Size: int64(len(files[full])), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := tw.Write(files[full]); err != nil {
			return err
		}
	}
	return tw.Close()
}

func (g guests) CopyIn(_ context.Context, name, dir string, r io.Reader) error {
	got := map[string][]byte{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		rel := path.Clean("/" + hdr.Name)
		got[path.Join(dir, rel)] = b
	}
	return g.h.change(g.op("CopyIn"), func() error {
		gu, d, err := g.running(name)
		if err != nil {
			return err
		}
		for full, b := range got {
			files, k := g.files(gu, d, full)
			files[k] = b
		}
		return nil
	})
}

// Dial answers on every port of a running guest, echoing what it reads.
func (g guests) Dial(_ context.Context, name string, _ int32) (net.Conn, error) {
	err := g.h.read(g.op("Dial"), func() error {
		_, _, err := g.running(name)
		return err
	})
	if err != nil {
		return nil, err
	}
	client, server := net.Pipe()
	go func() {
		defer func() { _ = server.Close() }()
		_, _ = io.Copy(server, server)
	}()
	return client, nil
}

// output is a command's stdout and stderr in the order they were written.
type output struct {
	chunks []chunk
}

type chunk struct {
	stderr bool
	b      []byte
}

func (o *output) stream(stderr bool) io.Writer {
	return writerFunc(func(p []byte) (int, error) {
		o.chunks = append(o.chunks, chunk{stderr: stderr, b: bytes.Clone(p)})
		return len(p), nil
	})
}

func (o *output) replay(stdout, stderr io.Writer) error {
	for _, c := range o.chunks {
		w := stdout
		if c.stderr {
			w = stderr
		}
		if w == nil {
			continue
		}
		if _, err := w.Write(c.b); err != nil {
			return err
		}
	}
	return nil
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
