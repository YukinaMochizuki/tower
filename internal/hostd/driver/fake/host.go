// Package fake is one host of all five drivers, in memory, for tests: a
// disk is a map of files, a guest runs a small set of commands on them
// (run), a mirror is a list of commits, the registry a set of references.
// Its faults are set by the test: any driver call failing, a guest that
// does not start, a full pool, a guest and its disk lost. With a state
// file it keeps the host there after every change and reads it back when
// made again, so a hostd restarted finds the host as a real one would, and
// another process can read what is on it (ReadState).
package fake

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// Defaults of a fake host.
const (
	DefaultPoolSize = 1 << 40
	// Overhead is what any disk or golden holds besides its files, as a
	// filesystem's metadata does.
	Overhead = 1 << 20
	// ImageSize is what an image pulled or built holds on the host.
	ImageSize = 1 << 28
	// Version is every fake driver's version.
	Version = "fake-1"
)

// Options are a fake host's.
type Options struct {
	// Path is the state file; empty keeps the host in memory alone.
	Path  string
	Clock clockwork.Clock
	// PoolSize zero is DefaultPoolSize.
	PoolSize uint64
	// Images are in the registry from the start.
	Images []string
}

// Host is the fake host. Its drivers are Drivers().
type Host struct {
	clock clockwork.Clock
	path  string

	mu sync.Mutex
	st state
	// The faults, never kept in the state file.
	failures   map[string]error
	startFails map[string]string
	free       *uint64
	health     string
	onExec     func(guest string, spec driver.ExecSpec) (int, bool)
	// holdPulls, while set, holds every pull of its references until it
	// is closed.
	holdPulls map[string]chan struct{}
	execs     []Exec
	// changed wakes the reads following a process, by guest and id.
	changed map[string]chan struct{}
}

// state is what the host holds, as its state file keeps it.
type state struct {
	PoolSize uint64
	Disks    map[string]*disk
	Goldens  map[string]*golden
	// Networks and Guests are keyed by kind and name: "container/x".
	Networks map[string]*network
	Guests   map[string]*guest
	Registry map[string]bool
	// Users are the users images of the registry run as, "" for root.
	Users map[string]string `json:",omitempty"`
	// Envs are the environments of the images built on the host.
	Envs   map[string][]string `json:",omitempty"`
	Pulled map[string]bool
	// Unpushed are the images built on the host and not pushed yet,
	// oldest first.
	Unpushed []string `json:",omitempty"`
	Mirrors  map[string]*repo
	Remotes  map[string]*repo
	// Addresses counts the addresses given out.
	Addresses int
}

type disk struct {
	Spec    driver.DiskSpec
	Files   map[string][]byte
	Base    int
	Created time.Time
}

type golden struct {
	Kind     driver.DiskKind
	Files    map[string][]byte
	Labels   driver.Labels
	Created  time.Time
	Deferred bool
}

type network struct {
	Labels  driver.Labels
	Created time.Time
}

type guest struct {
	Spec      driver.GuestSpec
	Image     string
	Running   bool
	StartedAt time.Time
	Address   string
	Created   time.Time
	// Files are a container's outside /workspace, which its disk holds.
	Files map[string][]byte
	// Env is a container's environment, every command's in it.
	Env map[string]string `json:",omitempty"`
	// Procs are its processes, oldest first; ProcN numbers them, and Keep
	// is how many finished ones it keeps.
	Procs []*proc
	ProcN int
	Keep  int
}

// repo is a remote repository or a mirror of it: its commits on its one
// branch, oldest first, and the files at its head.
type repo struct {
	Branch  string
	Commits []string
	Files   map[string][]byte
}

func (r *repo) head() string {
	if len(r.Commits) == 0 {
		return ""
	}
	return r.Commits[len(r.Commits)-1]
}

// Exec is a command a guest ran.
type Exec struct {
	Guest    string
	Command  []string
	Env      map[string]string
	ExitCode int
	// Process says it was started as a process, apart from any call.
	Process bool
}

// New makes a fake host, reading its state file if there is one.
func New(opts Options) (*Host, error) {
	h := &Host{
		clock: cmp.Or[clockwork.Clock](opts.Clock, clockwork.NewRealClock()), path: opts.Path,
		failures: map[string]error{}, startFails: map[string]string{}, health: "ONLINE",
		st: state{
			PoolSize: cmp.Or(opts.PoolSize, DefaultPoolSize), Disks: map[string]*disk{}, Goldens: map[string]*golden{},
			Networks: map[string]*network{}, Guests: map[string]*guest{}, Registry: map[string]bool{}, Pulled: map[string]bool{},
			Mirrors: map[string]*repo{}, Remotes: map[string]*repo{},
		},
	}
	for _, ref := range opts.Images {
		h.st.Registry[ref] = true
	}
	if h.path == "" {
		return h, nil
	}
	b, err := os.ReadFile(h.path)
	if errors.Is(err, fs.ErrNotExist) {
		return h, h.saveLocked()
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &h.st); err != nil {
		return nil, fmt.Errorf("fake host %s: %w", h.path, err)
	}
	return h, nil
}

// Drivers are the host's five drivers and its host info.
func (h *Host) Drivers() driver.Drivers {
	return driver.Drivers{
		Storage: storage{h}, Containers: containers{guests{h, "container"}}, VMs: vms{guests{h, "vm"}},
		Images: images{h}, Git: git{h}, Host: info{h},
	}
}

// saveLocked writes the state file, if there is one. The caller holds
// h.mu.
func (h *Host) saveLocked() error {
	if h.path == "" {
		return nil
	}
	b, err := json.Marshal(&h.st)
	if err != nil {
		return err
	}
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, h.path)
}

// change runs fn on the state under the lock, then keeps it.
func (h *Host) change(op string, fn func() error) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.failures[op]; err != nil {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	return h.saveLocked()
}

// read runs fn on the state under the lock.
func (h *Host) read(op string, fn func() error) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.failures[op]; err != nil {
		return err
	}
	return fn()
}

// The faults.

// Fail makes every call of op, such as "containers.Start" or
// "storage.Disks", fail with err, until cleared with nil.
func (h *Host) Fail(op string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err == nil {
		delete(h.failures, op)
	} else {
		h.failures[op] = err
	}
}

// FailStart makes the guest of the name fail to start, saying why, until
// cleared with an empty why.
func (h *Host) FailStart(name, why string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if why == "" {
		delete(h.startFails, name)
	} else {
		h.startFails[name] = why
	}
}

// SetFree fixes what the pool has free, as a full pool does; nil counts it
// from what it holds again.
func (h *Host) SetFree(free *uint64) { h.mu.Lock(); h.free = free; h.mu.Unlock() }

// HoldPulls holds every pull of ref, as a slow registry does, until the
// returned release is called.
func (h *Host) HoldPulls(ref string) (release func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.holdPulls == nil {
		h.holdPulls = map[string]chan struct{}{}
	}
	c := make(chan struct{})
	h.holdPulls[ref] = c
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.holdPulls[ref] == c {
			delete(h.holdPulls, ref)
			close(c)
		}
	}
}

// SetHealth sets the pool's health.
func (h *Host) SetHealth(health string) { h.mu.Lock(); h.health = health; h.mu.Unlock() }

// Lose makes a guest of kind ("container" or "vm") disappear from the host
// with its disk, as a host that lost them would.
func (h *Host) Lose(kind, name string) {
	_ = h.change("", func() error {
		if g := h.st.Guests[kind+"/"+name]; g != nil {
			delete(h.st.Disks, diskName(g.Spec.Disk))
			guests{h, kind}.endProcs(g, syscall.SIGKILL)
		}
		delete(h.st.Guests, kind+"/"+name)
		return nil
	})
}

// OnExec, when set, runs before a guest's own commands: a command it
// handles exits with the code it returns.
func (h *Host) OnExec(fn func(guest string, spec driver.ExecSpec) (int, bool)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onExec = fn
}

// AddImage puts a reference in the registry.
func (h *Host) AddImage(ref string) {
	_ = h.change("", func() error { h.st.Registry[ref] = true; return nil })
}

// InRegistry reports whether the registry holds ref.
func (h *Host) InRegistry(ref string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.st.Registry[ref]
}

// SetImageUser puts a reference in the registry, running as user.
func (h *Host) SetImageUser(ref, user string) {
	_ = h.change("", func() error {
		h.st.Registry[ref] = true
		if h.st.Users == nil {
			h.st.Users = map[string]string{}
		}
		h.st.Users[ref] = user
		return nil
	})
}

// SetRemote makes a repository at url with commits on branch, oldest
// first, holding files at its head.
func (h *Host) SetRemote(url, branch string, files map[string]string, commits ...string) {
	r := &repo{Branch: branch, Commits: commits, Files: map[string][]byte{}}
	for name, content := range files {
		r.Files[name] = []byte(content)
	}
	_ = h.change("", func() error { h.st.Remotes[url] = r; return nil })
}

// What the test reads back.

// Execs are the commands run so far, in order.
func (h *Host) Execs() []Exec {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.execs)
}

// RemoteHead is the commit a remote's branch is at.
func (h *Host) RemoteHead(url string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r := h.st.Remotes[url]; r != nil {
		return r.head()
	}
	return ""
}

// State is what is on a host, by name.
type State struct {
	Disks, Goldens, Containers, VMs, Networks, Mirrors, Images []string
}

// Empty reports whether nothing an instance or a golden accounts for is
// on the host: mirrors and the images pulled are caches of no instance.
func (s State) Empty() bool {
	return len(s.Disks)+len(s.Goldens)+len(s.Containers)+len(s.VMs)+len(s.Networks) == 0
}

// State is what is on the host now.
func (h *Host) State() State {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.st.summary()
}

// ReadState reads what is on the host whose state file is at path, as
// another process keeps it.
func ReadState(path string) (State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return State{}, err
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, fmt.Errorf("%s: %w", path, err)
	}
	return st.summary(), nil
}

func (st *state) summary() State {
	var s State
	s.Disks = slices.Sorted(maps.Keys(st.Disks))
	s.Goldens = slices.Sorted(maps.Keys(st.Goldens))
	for _, k := range slices.Sorted(maps.Keys(st.Guests)) {
		kind, name, _ := cutKey(k)
		if kind == "vm" {
			s.VMs = append(s.VMs, name)
		} else {
			s.Containers = append(s.Containers, name)
		}
	}
	s.Networks = slices.Sorted(maps.Keys(st.Networks))
	s.Mirrors = slices.Sorted(maps.Keys(st.Mirrors))
	s.Images = slices.Sorted(maps.Keys(st.Pulled))
	return s
}

func cutKey(k string) (kind, name string, ok bool) { return strings.Cut(k, "/") }

// used is what the pool holds. The caller holds h.mu.
func (h *Host) used() uint64 {
	var n uint64
	for _, d := range h.st.Disks {
		n += Overhead + uint64(max(0, filesSize(d.Files)-d.Base))
	}
	for _, g := range h.st.Goldens {
		n += Overhead + uint64(filesSize(g.Files))
	}
	n += uint64(len(h.st.Pulled)) * ImageSize
	return n
}

// freeLocked is what the pool has free. The caller holds h.mu.
func (h *Host) freeLocked() uint64 {
	if h.free != nil {
		return *h.free
	}
	return h.st.PoolSize - min(h.st.PoolSize, h.used())
}

// room fails with ErrNoSpace on a pool with less than need free. The
// caller holds h.mu.
func (h *Host) room(need uint64) error {
	if h.freeLocked() < need {
		return fmt.Errorf("%d bytes free, %d needed: %w", h.freeLocked(), need, driver.ErrNoSpace)
	}
	return nil
}

func filesSize(files map[string][]byte) int {
	n := 0
	for _, b := range files {
		n += len(b)
	}
	return n
}

// diskName is the disk a guest's Disk path names.
func diskName(path string) string { return filepath.Base(path) }

// diskPath is where a disk's guest finds it.
func diskPath(name string) string { return "/fake/pool/" + name }

type info struct{ h *Host }

func (i info) Info(context.Context) (driver.HostInfo, error) {
	var running uint64
	i.h.mu.Lock()
	defer i.h.mu.Unlock()
	if err := i.h.failures["host.Info"]; err != nil {
		return driver.HostInfo{}, err
	}
	for _, g := range i.h.st.Guests {
		if g.Running {
			running += g.Spec.Memory
		}
	}
	const total = 64 << 30
	return driver.HostInfo{MemoryTotal: total, MemoryFree: total - min(total, running), CPUs: 16}, nil
}
