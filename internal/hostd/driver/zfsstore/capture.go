package zfsstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// captureSnapshot names the snapshot of a volume a CustomImage is captured
// from.
func captureSnapshot(imageUID string) string { return "customimage-" + imageUID }

func (s *Store) SnapshotVolume(ctx context.Context, diskUID, imageUID string) error {
	vol, err := s.volumeName(diskUID)
	if err != nil {
		return err
	}
	if !uidRE.MatchString(imageUID) {
		return fmt.Errorf("%q cannot name a custom image", imageUID)
	}
	defer s.lock(driver.CustomImageBase(imageUID))()
	disk, _, _, ok, err := volumeOf(ctx, vol)
	switch {
	case err != nil:
		return err
	case !ok:
		return fmt.Errorf("disk %s has no volume", diskUID)
	case disk != diskUID:
		return fmt.Errorf("%s is not disk %s's volume; left alone", vol, diskUID)
	}
	snap := vol + "@" + captureSnapshot(imageUID)
	p, ok, err := props(ctx, snap, propCustomImage)
	switch {
	case err != nil:
		return err
	case ok && p[propCustomImage] != imageUID:
		return fmt.Errorf("%s exists and is not custom image %s's; left alone", snap, imageUID)
	case ok:
		return nil
	}
	_, err = zfs(ctx, "snapshot", "-o", propCustomImage+"="+imageUID, snap)
	return err
}

func (s *Store) Captures(ctx context.Context) ([]driver.Capture, error) {
	vols, err := s.Volumes(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := list(ctx, "snapshot", s.disks(), "name", propCustomImage)
	if err != nil {
		return nil, err
	}
	var out []driver.Capture
	for _, row := range rows {
		vol, name, _ := strings.Cut(row[0], "@")
		image := unset(row[1])
		disk := strings.TrimPrefix(vol, s.disks()+"/")
		ours := slices.ContainsFunc(vols, func(v driver.Volume) bool { return v.DiskUID == disk })
		if !ours || !uidRE.MatchString(image) || name != captureSnapshot(image) {
			continue // not ours
		}
		out = append(out, driver.Capture{ImageUID: image, DiskUID: disk})
	}
	return out, nil
}

// captureOf returns the snapshot taken for the CustomImage of imageUID, or
// "" if there is none.
func (s *Store) captureOf(ctx context.Context, imageUID string) (string, error) {
	captures, err := s.Captures(ctx)
	if err != nil {
		return "", err
	}
	for _, c := range captures {
		if c.ImageUID == imageUID {
			return s.disks() + "/" + c.DiskUID + "@" + captureSnapshot(imageUID), nil
		}
	}
	return "", nil
}

func (s *Store) DeleteCapture(ctx context.Context, imageUID string) error {
	defer s.lock(driver.CustomImageBase(imageUID))()
	snap, err := s.captureOf(ctx, imageUID)
	if err != nil || snap == "" {
		return err
	}
	return destroy(ctx, snap)
}

// imageBase reads the finished base b of a CustomImage; ok is false if it
// is not there or not finished: its @base without b's property set on
// itself, which it would otherwise inherit from its volume.
func imageBase(ctx context.Context, b vmBase) (driver.ImageBase, bool, error) {
	mark, ok, err := localProps(ctx, b.snapshot(), b.prop)
	if err != nil || !ok || mark[b.prop] != b.value {
		return driver.ImageBase{}, false, err
	}
	p, ok, err := props(ctx, b.snapshot(), "volsize", "logicalreferenced")
	if err != nil || !ok {
		return driver.ImageBase{}, false, err
	}
	size, err1 := strconv.ParseInt(p["volsize"], 10, 64)
	used, err2 := strconv.ParseInt(p["logicalreferenced"], 10, 64)
	if err := errors.Join(err1, err2); err != nil {
		return driver.ImageBase{}, false, fmt.Errorf("%s: %w", b.snapshot(), err)
	}
	return driver.ImageBase{Key: b.key, Size: size, Used: used}, true, nil
}

func (s *Store) CaptureImageBase(ctx context.Context, imageUID string) (driver.ImageBase, error) {
	b, err := s.vmBaseOf(driver.CustomImageBase(imageUID))
	if err != nil {
		return driver.ImageBase{}, err
	}
	defer s.lock(b.key)()
	if base, ok, err := imageBase(ctx, b); err != nil || ok {
		return base, err
	}
	// An unfinished base of ours is made again; anything else at its name
	// is left alone.
	if p, ok, err := localProps(ctx, b.vol, b.prop); err != nil {
		return driver.ImageBase{}, err
	} else if ok {
		if p[b.prop] != b.value {
			return driver.ImageBase{}, fmt.Errorf("%s exists and is not a base of custom image %s", b.vol, imageUID)
		}
		if err := destroyBase(ctx, b); err != nil {
			return driver.ImageBase{}, err
		}
	}
	snap, err := s.captureOf(ctx, imageUID)
	if err != nil {
		return driver.ImageBase{}, err
	}
	if snap == "" {
		return driver.ImageBase{}, fmt.Errorf("custom image %s: %w", imageUID, driver.ErrNoCapture)
	}
	p, _, err := props(ctx, snap, "referenced")
	if err != nil {
		return driver.ImageBase{}, err
	}
	need, err := strconv.ParseInt(p["referenced"], 10, 64)
	if err != nil {
		return driver.ImageBase{}, fmt.Errorf("%s: referenced %q", snap, p["referenced"])
	}
	c, err := s.Capacity(ctx)
	if err != nil {
		return driver.ImageBase{}, err
	}
	if c.Free < need {
		return driver.ImageBase{}, &driver.NoSpaceError{What: "the base of custom image " + imageUID, Room: driver.StorageRoom, Need: need, Free: c.Free}
	}
	finish := func() error {
		// Received with its snapshot, @base, and its volume's property,
		// which @base only inherits; the same property set on @base itself,
		// last, says it is finished.
		if err := sendReceive(ctx, snap, b.snapshot(), b.prop+"="+b.value); err != nil {
			return fmt.Errorf("copying %s into the base of custom image %s: %w", snap, imageUID, err)
		}
		if _, err := zfs(ctx, "set", "readonly=on", b.vol); err != nil {
			return err
		}
		_, err := zfs(ctx, "set", b.prop+"="+b.value, b.snapshot())
		return err
	}
	if err := finish(); err != nil {
		// Unfinished, nothing can use it. Only a volume of ours, received
		// with its property, is destroyed.
		if p, ok, perr := localProps(context.WithoutCancel(ctx), b.vol, b.prop); perr == nil && ok && p[b.prop] == b.value {
			_ = destroyBase(context.WithoutCancel(ctx), b)
		}
		return driver.ImageBase{}, err
	}
	if err := destroy(ctx, snap); err != nil {
		s.cfg.Log.Warn("the base of a custom image is made, and its source's snapshot stays", "snapshot", snap, "error", err)
	}
	base, ok, err := imageBase(ctx, b)
	if err == nil && !ok {
		err = fmt.Errorf("the base of custom image %s is not there once made", imageUID)
	}
	return base, err
}

func (s *Store) FillImageBase(ctx context.Context, imageUID string, size int64, fill func(dev string) error) (driver.ImageBase, error) {
	b, err := s.vmBaseOf(driver.CustomImageBase(imageUID))
	if err != nil {
		return driver.ImageBase{}, err
	}
	defer s.lock(b.key)()
	if base, ok, err := imageBase(ctx, b); err != nil || ok {
		return base, err
	}
	// An unfinished base of ours is made again; anything else at its name
	// is left alone.
	if p, ok, err := localProps(ctx, b.vol, b.prop); err != nil {
		return driver.ImageBase{}, err
	} else if ok {
		if p[b.prop] != b.value {
			return driver.ImageBase{}, fmt.Errorf("%s exists and is not a base of custom image %s", b.vol, imageUID)
		}
		if err := destroyBase(ctx, b); err != nil {
			return driver.ImageBase{}, err
		}
	}
	size = roundUp(max(size, 1), align)
	release, err := s.reserveFill(ctx, imageUID, size)
	if err != nil {
		return driver.ImageBase{}, err
	}
	defer release()
	if _, err := zfs(ctx, "create", "-s", "-V", strconv.FormatInt(size, 10),
		"-o", "volmode=dev", "-o", b.prop+"="+b.value, b.vol); err != nil {
		return driver.ImageBase{}, err
	}
	finish := func() error {
		// The zvol's link, once it leads to the zvol: what a VM is given,
		// as its launcher checks it is the zvol it names.
		if _, err := s.waitDev(ctx, b.vol); err != nil {
			return err
		}
		if err := fill(device(b.vol)); err != nil {
			return fmt.Errorf("filling the base of custom image %s: %w", imageUID, err)
		}
		// @base inherits the volume's property; the same property set on
		// @base itself, last, says it is finished, as for a captured base.
		if _, err := zfs(ctx, "snapshot", b.snapshot()); err != nil {
			return err
		}
		if _, err := zfs(ctx, "set", "readonly=on", b.vol); err != nil {
			return err
		}
		_, err = zfs(ctx, "set", b.prop+"="+b.value, b.snapshot())
		return err
	}
	if err := finish(); err != nil {
		_ = destroyBase(context.WithoutCancel(ctx), b)
		return driver.ImageBase{}, err
	}
	base, ok, err := imageBase(ctx, b)
	if err == nil && !ok {
		err = fmt.Errorf("the base of custom image %s is not there once made", imageUID)
	}
	return base, err
}

// sendReceive copies the snapshot snap whole into target, a snapshot of a
// volume that does not exist, made with the user property prop: zfs send,
// without its properties, piped into zfs receive. The pipe's ends are the
// two commands' alone, so either ending ends the other: a receive that
// fails breaks the send's pipe. A send that fails is the cause of the
// receive's failing on the short stream, and is reported as it.
func sendReceive(ctx context.Context, snap, target, prop string) error {
	var sendErr, recvErr bytes.Buffer
	send := exec.CommandContext(ctx, "zfs", "send", snap)
	recv := exec.CommandContext(ctx, "zfs", "receive", "-o", prop, "-o", "refreservation=none", target)
	for _, cmd := range []*exec.Cmd{send, recv} {
		cmd.Env = append(os.Environ(), "LC_ALL=C")
	}
	send.Stderr, recv.Stderr = &sendErr, &recvErr
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	recv.Stdin, send.Stdout = r, w
	err = recv.Start()
	_ = r.Close()
	if err != nil {
		_ = w.Close()
		return err
	}
	err = send.Start()
	_ = w.Close()
	if err != nil {
		_ = recv.Process.Kill()
		_ = recv.Wait()
		return err
	}
	errRecv := recv.Wait()
	errSend := send.Wait()
	sendMsg, recvMsg := strings.TrimSpace(sendErr.String()), strings.TrimSpace(recvErr.String())
	noSpace := func(msg string) bool {
		return strings.Contains(msg, "out of space") || strings.Contains(msg, "No space left")
	}
	switch {
	case errSend != nil && !brokenPipe(errSend, sendMsg):
		err := fmt.Errorf("zfs send %s: %w: %s", snap, errSend, sendMsg)
		if noSpace(sendMsg) {
			err = fmt.Errorf("%w: %w", err, driver.ErrNoSpace)
		}
		return err
	case errRecv != nil && noSpace(recvMsg):
		return fmt.Errorf("zfs receive %s: %s: %w", target, recvMsg, driver.ErrNoSpace)
	case errRecv != nil:
		return fmt.Errorf("zfs receive %s: %w: %s", target, errRecv, recvMsg)
	case errSend != nil:
		return fmt.Errorf("zfs send %s: %w: %s", snap, errSend, sendMsg)
	}
	return nil
}

// brokenPipe says a send ended because what it wrote to was gone: killed
// by SIGPIPE, or saying so.
func brokenPipe(err error, msg string) bool {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGPIPE {
			return true
		}
	}
	return strings.Contains(msg, "Broken pipe")
}

// reserveFill counts size bytes toward the fills under way, as long as
// the pool has room for them besides the others, and returns what stops
// counting them. The fill writes all but the zeros, so a base may take up
// to its size: how much less is not known before. A fill counts at its
// whole size until it ends, though part of it is written meanwhile, so a
// fill may be refused that would have fit, but none is let in that does
// not.
func (s *Store) reserveFill(ctx context.Context, imageUID string, size int64) (release func(), err error) {
	s.fillMu.Lock()
	defer s.fillMu.Unlock()
	c, err := s.Capacity(ctx)
	if err != nil {
		return nil, err
	}
	if free := c.Free - s.filling; free < size {
		return nil, &driver.NoSpaceError{What: "the base of custom image " + imageUID, Room: driver.StorageRoom, Need: size, Free: max(free, 0)}
	}
	s.filling += size
	return func() {
		s.fillMu.Lock()
		defer s.fillMu.Unlock()
		s.filling -= size
	}, nil
}
