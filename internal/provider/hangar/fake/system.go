package fake

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	hangarcompute "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	hangarmeta "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	hangarstorage "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"

	"github.com/YukinaMochizuki/tower/internal/guest/guestbin"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
	"github.com/YukinaMochizuki/tower/internal/system"
)

// systemBinary is tower-system in the system image.
const systemBinary = "/usr/local/bin/tower-system"

// A custom image is the workspace of the container that mounts its source
// disk, copied as it is captured, Ready at once; a container mounting a
// disk made from it starts with that copy.

// imagesDir holds the images' copies.
func (f *Fake) imagesDir(uid string) string { return filepath.Join(f.dir, "images", uid) }

// imageApplied captures a new image from its source disk. f.mu is held.
func (f *Fake) imageApplied(img *hangarstorage.CustomImage, isNew bool) error {
	if !isNew {
		return nil
	}
	disk := img.GetSpec().GetSource().GetDisk().GetName()
	d, ok := f.disks.objs[disk]
	if !ok {
		return connectError("failed_precondition", fmt.Sprintf("disk %s is not there", disk))
	}
	for _, ct := range f.containers.objs {
		for _, v := range ct.GetSpec().GetVolumes() {
			if v.GetSource() != disk {
				continue
			}
			if m := f.machines["ct-"+ct.GetMetadata().GetName()]; m.GetStatus().GetPhase() == hangarcompute.MachinePhase_MACHINE_PHASE_RUNNING {
				return connectError("failed_precondition", fmt.Sprintf("disk %s's machine runs", disk))
			}
			if err := copyDir(filepath.Join(f.root(ct.GetMetadata().GetUid()), v.GetTarget()), f.imagesDir(img.GetMetadata().GetUid())); err != nil {
				return err
			}
		}
	}
	// A VM's root disk is its whole filesystem.
	for _, vm := range f.vms.objs {
		if vm.GetSpec().GetRootDisk().GetDisk() != disk {
			continue
		}
		if m := f.machines["vm-"+vm.GetMetadata().GetName()]; m.GetStatus().GetPhase() == hangarcompute.MachinePhase_MACHINE_PHASE_RUNNING {
			return connectError("failed_precondition", fmt.Sprintf("disk %s's machine runs", disk))
		}
	}
	if _, err := os.Stat(f.diskDir(d.GetMetadata().GetUid())); err == nil {
		if err := copyDir(f.diskDir(d.GetMetadata().GetUid()), f.imagesDir(img.GetMetadata().GetUid())); err != nil {
			return err
		}
	}
	img.Status = &hangarstorage.CustomImageStatus{
		DiskSize: d.GetSpec().GetSize(), UsedSize: "1Gi",
		Conditions: []*hangarmeta.Condition{{Type: "Ready", Status: hangarmeta.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Ready"}},
	}
	return nil
}

func (f *Fake) imageDeleting(img *hangarstorage.CustomImage) error {
	return os.RemoveAll(f.imagesDir(img.GetMetadata().GetUid()))
}

// fillVolumes copies into a new container's volumes the images their disks
// are made from. f.mu is held.
func (f *Fake) fillVolumes(ct *hangarcompute.Container) error {
	for _, v := range ct.GetSpec().GetVolumes() {
		src := f.disks.objs[v.GetSource()].GetSpec().GetSource().GetImage()
		if src.GetName() == "" {
			continue
		}
		img, ok := f.images.objs[src.GetName()]
		if !ok {
			return connectError("failed_precondition", fmt.Sprintf("custom image %s is not there", src.GetName()))
		}
		if err := copyDir(f.imagesDir(img.GetMetadata().GetUid()), filepath.Join(f.root(ct.GetMetadata().GetUid()), v.GetTarget())); err != nil {
			return err
		}
	}
	return nil
}

// copyDir copies what from holds into to, which it makes; from not there
// copies nothing.
func copyDir(from, to string) error {
	if err := os.MkdirAll(to, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(from, func(p string, d os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
}

// system runs a call of tower-system in the system container: the real
// one (System, built by the test) for the mirrors, over a directory of the
// container's, and the fake's own for what BuildKit and the tailnet do. A
// build's digest is a hash of its context, and the image it names runs as
// the Dockerfile's last USER.
func (f *Fake) system(ctx context.Context, name, uid string, stdin io.Reader, stdout, stderr io.Writer) (hangar.ExecExit, error) {
	in := bufio.NewReader(stdin)
	typ, data, err := guestbin.ReadFrame(in)
	if err != nil || typ != guestbin.FrameHeader {
		return hangar.ExecExit{}, fmt.Errorf("the call's header: %w", err)
	}
	var h system.Header
	if err := json.Unmarshal(data, &h); err != nil {
		return hangar.ExecExit{}, err
	}
	f.mu.Lock()
	registry := ""
	for _, e := range f.containers.objs[name].GetSpec().GetEnv() {
		if e.GetName() == "TOWER_REGISTRY" {
			registry = e.GetValue()
		}
	}
	f.mu.Unlock()
	answer := func(r system.Result) (hangar.ExecExit, error) {
		b, _ := json.Marshal(r)
		_, err := stdout.Write(append(b, '\n'))
		return hangar.ExecExit{}, err
	}
	fail := func(code int, msg string) (hangar.ExecExit, error) {
		_, _ = fmt.Fprintln(stderr, "tower-system: "+msg)
		return hangar.ExecExit{Code: code}, nil
	}
	switch h.Op {
	case system.OpStatus:
		// Whether the server answers, as the real status asks it.
		srv, err := f.startServer(uid, registry)
		return answer(system.Result{BuildKit: "fake", Serving: err == nil && controlAnswers(srv.control)})
	case system.OpTailnetUp:
		return answer(system.Result{Tailnet: true})
	case system.OpResolve:
		f.mu.Lock()
		user, built := f.built[h.Image]
		f.mu.Unlock()
		_, digest, ok := strings.Cut(h.Image, "@")
		if !ok && !built {
			return fail(system.ExitNotFound, h.Image+" is not in the registry")
		}
		return answer(system.Result{Digest: digest, User: user})
	case system.OpBuild:
		dockerfile := h.Dockerfile
		if dockerfile == "" {
			dockerfile = path.Join(h.Dir, "Dockerfile")
		}
		sum := sha256.New()
		var content []byte
		if h.Mirror != "" {
			// The Dockerfile at the ref, as the real tower-system reads it.
			var out bytes.Buffer
			code, err := f.runSystem(ctx, uid, registry, system.Header{Op: system.OpRead, Mirror: h.Mirror, Ref: h.Ref, Paths: []string{dockerfile}, MaxContent: 1 << 20}, nil, &out, stderr)
			if err != nil || code != 0 {
				return hangar.ExecExit{Code: code}, err
			}
			var res system.Result
			if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &res); err != nil {
				return hangar.ExecExit{}, err
			}
			e, ok := res.Entries[dockerfile]
			if !ok || e.Dir {
				return fail(system.ExitFailed, "building: no Dockerfile "+dockerfile)
			}
			content = e.Content
			_, _ = sum.Write([]byte(res.Commit))
		} else {
			var ctxTar bytes.Buffer
			if err := guestbin.ReadData(in, &ctxTar); err != nil {
				return hangar.ExecExit{}, err
			}
			_, _ = sum.Write(ctxTar.Bytes())
			tr := tar.NewReader(&ctxTar)
			for {
				hdr, err := tr.Next()
				if err != nil {
					break
				}
				if path.Clean(hdr.Name) == path.Clean(dockerfile) {
					content, _ = io.ReadAll(tr)
				}
			}
			if content == nil {
				return fail(system.ExitFailed, "building: no Dockerfile "+dockerfile)
			}
		}
		_, _ = sum.Write([]byte(h.Repository + "\x00" + dockerfile))
		image := registry + "/" + h.Repository + "@sha256:" + hex.EncodeToString(sum.Sum(nil))
		user := ""
		for _, line := range strings.Split(string(content), "\n") {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "USER "); ok {
				user = strings.TrimSpace(rest)
			}
		}
		f.mu.Lock()
		if f.built == nil {
			f.built = map[string]string{}
		}
		f.built[image] = user
		f.mu.Unlock()
		return answer(system.Result{Image: image})
	}
	code, err := f.runSystem(ctx, uid, registry, h, in, stdout, stderr)
	return hangar.ExecExit{Code: code}, err
}

// runSystem runs the real tower-system with the call h, the rest of its
// stdin after the header from rest.
func (f *Fake) runSystem(ctx context.Context, uid, registry string, h system.Header, rest io.Reader, stdout, stderr io.Writer) (int, error) {
	if f.System == "" {
		return 0, errors.New("the fake has no tower-system to run")
	}
	b, err := json.Marshal(h)
	if err != nil {
		return 0, err
	}
	var hdr bytes.Buffer
	if err := guestbin.WriteFrame(&hdr, guestbin.FrameHeader, b); err != nil {
		return 0, err
	}
	if rest == nil {
		_ = guestbin.WriteFrame(&hdr, guestbin.FrameEOF, nil)
		rest = strings.NewReader("")
	}
	cmd := exec.CommandContext(ctx, f.System, "call")
	cmd.Env = append(os.Environ(), "TOWER_SYSTEM_ROOT="+filepath.Join(f.root(uid), "var", "lib", "tower"), "TOWER_REGISTRY="+registry)
	f.mu.Lock()
	if s, ok := f.servers[uid]; ok {
		cmd.Env = append(cmd.Env, "TOWER_SYSTEM_CONTROL="+s.control)
	}
	f.mu.Unlock()
	cmd.Stdout, cmd.Stderr = stdout, stderr
	pipe, err := cmd.StdinPipe()
	if err != nil {
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	// Its stdin is copied apart from the call, which keeps it open until
	// it ends: waiting for it would wait for the call.
	go func() {
		_, _ = io.Copy(pipe, io.MultiReader(&hdr, rest))
		_ = pipe.Close()
	}()
	err = cmd.Wait()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return 0, err
}
