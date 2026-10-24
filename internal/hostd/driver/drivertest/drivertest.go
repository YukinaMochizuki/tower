// Package drivertest is the contract of hostd's drivers: a suite per
// driver, run against the fake host in CI and against a host's real
// drivers on a testbed. Everything a suite makes is named for its test
// and carries the suite's labels, and is removed when the test ends, so
// the suites run on a host that holds other things.
package drivertest

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// Harness is a host's drivers, and what the suites need of the host.
type Harness struct {
	Drivers driver.Drivers
	// Image is a reference by digest the registry holds, whose containers
	// run the commands the suites name (true, false, exit, echo, env, cat,
	// emit, sh -c, sleep; see the fake's) and echo what they read on
	// EchoPort; MissingImage, one the registry lacks.
	Image, MissingImage string
	// BaseImage is what a VM boots, whose guest does the same.
	BaseImage string
	EchoPort  int32
	// Remote is a repository the git driver may fetch and push to, on
	// Branch; RemoteHead, if set, is the commit Branch is at.
	Remote, Branch, RemoteHead string
}

// Run runs every driver's suite.
func Run(t *testing.T, h Harness) {
	t.Run("Storage", func(t *testing.T) { RunStorage(t, h) })
	t.Run("Containers", func(t *testing.T) { RunGuests(t, h, h.containers()) })
	t.Run("VMs", func(t *testing.T) { RunGuests(t, h, h.vms()) })
	t.Run("Images", func(t *testing.T) { RunImages(t, h) })
	t.Run("Git", func(t *testing.T) { RunGit(t, h) })
}

// labels are the suites' own, on everything they make.
func labels(t *testing.T) driver.Labels {
	return driver.Labels{driver.LabelOwner: "drivertest", driver.LabelInstance: name(t, "")}
}

// name is a name for what the test makes, unique to it.
func name(t *testing.T, what string) string {
	sum := sha256.Sum256([]byte(t.Name()))
	n := "dt-" + hex.EncodeToString(sum[:])[:10]
	if what != "" {
		n += "-" + what
	}
	return n
}

// mustNot fails the test unless err is nil.
func mustNot(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// is fails the test unless err is target.
func is(t *testing.T, what string, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Errorf("%s: %v, want %v", what, err, target)
	}
}

// files reads a tar's regular files by name.
func files(t *testing.T, b []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		mustNot(t, "reading a tar", err)
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		mustNot(t, "reading a tar", err)
		out[hdr.Name] = string(data)
	}
}

// tarOf is a tar of files.
func tarOf(t *testing.T, files map[string]string) io.Reader {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for name, content := range files {
		mustNot(t, "writing a tar", w.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}))
		_, err := io.WriteString(w, content)
		mustNot(t, "writing a tar", err)
	}
	mustNot(t, "writing a tar", w.Close())
	return &b
}

func describe(d driver.Disk) string { return fmt.Sprintf("%+v", d) }
