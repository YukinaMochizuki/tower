package guestops_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/YukinaMochizuki/tower/internal/guest"
	"github.com/YukinaMochizuki/tower/internal/guest/guestops"
)

// towerGuest is tower-guest, built once for the package's tests.
var towerGuest string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tower-guest-")
	if err != nil {
		panic(err)
	}
	towerGuest = filepath.Join(dir, "tower-guest")
	build := exec.Command("go", "build", "-o", towerGuest, "github.com/YukinaMochizuki/tower/cmd/tower-guest")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// local runs tower-guest shim here, as a guest would, its own process.
type local struct{}

func (local) RunShim(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd := exec.Command(towerGuest, "shim")
	cmd.Stdout, cmd.Stderr = stdout, stderr
	pipe, err := cmd.StdinPipe()
	if err != nil {
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	// Its stdin is copied apart from the call, and ends with it, as a
	// connection's does: how the call's end reaches it.
	go func() {
		_, _ = io.Copy(pipe, stdin)
		_ = pipe.Close()
	}()
	go func() {
		<-ctx.Done()
		_ = pipe.Close()
	}()
	err = cmd.Wait()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		err = nil
	}
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	return cmd.ProcessState.ExitCode(), err
}

func ops(t *testing.T) guestops.Ops {
	root := t.TempDir()
	return guestops.Ops{R: local{}, ProcRoot: filepath.Join(root, "processes")}
}

func run(t *testing.T, o guestops.Ops, env map[string]string, stdin string, command ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code, err := o.Exec(t.Context(), guest.ExecSpec{Command: command, Env: env, Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errOut})
	if err != nil {
		t.Fatalf("running %v: %v", command, err)
	}
	return code, out.String(), errOut.String()
}

// TestExec: a command's output, exit code, stdin, environment of its own
// and directory; one that cannot start exits 127; one whose call ends is
// killed, its group with it.
func TestExec(t *testing.T) {
	o := ops(t)
	if code, out, _ := run(t, o, nil, "", "echo", "hello", "world"); code != 0 || out != "hello world\n" {
		t.Errorf("echo: %d %q", code, out)
	}
	if code, _, _ := run(t, o, nil, "", "sh", "-c", "exit 3"); code != 3 {
		t.Errorf("exit 3: %d", code)
	}
	if _, out, _ := run(t, o, nil, "typed", "cat"); out != "typed" {
		t.Errorf("cat: %q", out)
	}
	if _, out, _ := run(t, o, map[string]string{"GIVEN": "yes"}, "", "sh", "-c", "echo $GIVEN"); out != "yes\n" {
		t.Errorf("the environment given: %q", out)
	}
	dir := t.TempDir()
	var out bytes.Buffer
	if _, err := o.Exec(t.Context(), guest.ExecSpec{Command: []string{"pwd"}, Dir: dir, Stdout: &out}); err != nil || strings.TrimSpace(out.String()) != dir {
		t.Errorf("pwd in %s: %q, %v", dir, out.String(), err)
	}
	if code, _, errOut := run(t, o, nil, "", "no-such-command"); code != 127 || !strings.Contains(errOut, "no-such-command") {
		t.Errorf("a command that cannot start: %d %q", code, errOut)
	}
	marker := filepath.Join(t.TempDir(), "child")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := o.Exec(ctx, guest.ExecSpec{Command: []string{"sh", "-c", "sleep 3600 & echo $! > " + marker + "; wait"}})
		done <- err
	}()
	pid := waitPID(t, marker)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a command whose call ended returned no error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a command whose call ended still runs")
	}
	waitGone(t, pid)
}

func waitPID(t *testing.T, file string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(file)
		if err == nil && len(bytes.TrimSpace(b)) > 0 {
			var pid int
			for _, c := range bytes.TrimSpace(b) {
				pid = pid*10 + int(c-'0')
			}
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was never written", file)
		}
		<-time.After(10 * time.Millisecond)
	}
}

// waitGone fails the test unless pid ends within seconds.
func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		// A zombie of ours is gone too.
		if b, err := os.ReadFile(filepath.Join("/proc", itoa(pid), "stat")); err == nil && strings.Contains(string(b), ") Z ") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d is still alive", pid)
		}
		<-time.After(10 * time.Millisecond)
	}
}

func itoa(n int) string {
	var b []byte
	for n > 0 || len(b) == 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestFiles: a file written whole with its mode, then in a chunk where it
// ends, refused where it does not; read back; one not there.
func TestFiles(t *testing.T) {
	o, ctx := ops(t), t.Context()
	p := filepath.Join(t.TempDir(), "made", "a.txt")
	if err := o.WriteFile(ctx, p, 0o640, 0, strings.NewReader("cont")); err != nil {
		t.Fatal(err)
	}
	if err := o.WriteFile(ctx, p, 0, 4, strings.NewReader("ents")); err != nil {
		t.Fatal(err)
	}
	if err := o.WriteFile(ctx, p, 0, 2, strings.NewReader("xx")); !errors.Is(err, guest.ErrBadOffset) {
		t.Errorf("a chunk not where the file ends: %v", err)
	}
	var b bytes.Buffer
	if err := o.ReadFile(ctx, p, &b); err != nil || b.String() != "contents" {
		t.Errorf("read back %q, %v", b.String(), err)
	}
	if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o640 {
		t.Errorf("the file's mode: %v, %v", st.Mode(), err)
	}
	if err := o.ReadFile(ctx, p+".none", io.Discard); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("reading what is not there: %v", err)
	}
}

// TestCopy: a directory copied out as a tar named from its parent, and
// into another, with its link; a tar climbing out of its directory is
// refused, writing nothing outside it.
func TestCopy(t *testing.T) {
	o, ctx := ops(t), t.Context()
	src := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "result.txt"), []byte("built"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub/result.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	var tarball bytes.Buffer
	if err := o.CopyOut(ctx, src, &tarball); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := o.CopyIn(ctx, filepath.Join(dst, "in"), bytes.NewReader(tarball.Bytes())); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "in", "out", "link")); err != nil || string(b) != "built" {
		t.Errorf("the copy through its link holds %q, %v", b, err)
	}
	if err := o.CopyOut(ctx, src+"-none", io.Discard); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("copying out what is not there: %v", err)
	}
	// A tar whose link leads out, and an entry through it.
	var evil bytes.Buffer
	tw := tar.NewWriter(&evil)
	outside := t.TempDir()
	_ = tw.WriteHeader(&tar.Header{Name: "esc", Typeflag: tar.TypeSymlink, Linkname: outside})
	_ = tw.WriteHeader(&tar.Header{Name: "esc/pwned", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	if err := o.CopyIn(ctx, filepath.Join(dst, "evil"), &evil); err == nil {
		t.Error("a tar writing through a link out of its directory was taken")
	}
	if _, err := os.Stat(filepath.Join(outside, "pwned")); err == nil {
		t.Error("a tar wrote outside its directory")
	}
}

func startProc(t *testing.T, o guestops.Ops, spec guest.ProcessSpec) string {
	t.Helper()
	if spec.LogCap == 0 {
		spec.LogCap = 1 << 20
	}
	if spec.Keep == 0 {
		spec.Keep = 100
	}
	id, err := o.StartProcess(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func follow(t *testing.T, o guestops.Ops, id string) (string, guest.LogEnd) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var text strings.Builder
	end, err := o.ReadProcessLog(ctx, id, 0, true, func(c guest.LogChunk) error {
		text.Write(c.Data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return text.String(), end
}

// TestProcesses: a process outlives its call and writes its log; a capped
// log holds its last bytes; finished processes beyond those kept go; a
// signal reaches its group; its environment is in no file; listed and
// read through the shim and from the host alike.
func TestProcesses(t *testing.T) {
	o := ops(t)
	ctx, cancel := context.WithCancel(t.Context())
	id, err := o.StartProcess(ctx, guest.ProcessSpec{Name: "build", Command: []string{"sh", "-c", "echo one; sleep 0.05; echo two >&2; sleep 0.2; echo $SECRET_ONE"}, Env: map[string]string{"SECRET_ONE": "canary-proc-Aa11"}, LogCap: 1 << 20, Keep: 100})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	text, end := follow(t, o, id)
	if text != "one\ntwo\ncanary-proc-Aa11\n" || end.Exit == nil || end.Exit.Code != 0 || end.Next != int64(len(text)) {
		t.Errorf("followed %q, ended %+v", text, end)
	}
	_ = filepath.WalkDir(o.ProcRoot, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() != "lock" && !strings.HasPrefix(d.Name(), "log-") {
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte("canary-proc-Aa11")) {
				t.Errorf("%s holds the process's environment", p)
			}
		}
		return nil
	})

	capped := startProc(t, o, guest.ProcessSpec{Command: []string{"sh", "-c", "echo 0123456789; sleep 0.05; echo abcdefghij; sleep 0.05; echo ABCDEFGHIJ"}, LogCap: 16})
	follow(t, o, capped)
	var got strings.Builder
	end, err = o.ReadProcessLog(t.Context(), capped, 0, false, func(c guest.LogChunk) error { got.Write(c.Data); return nil })
	if err != nil || got.String() != "ghij\nABCDEFGHIJ\n" || end.HeldFrom != 17 || end.Next != 33 {
		t.Errorf("a capped log: %q, %+v, %v", got.String(), end, err)
	}

	group := startProc(t, o, guest.ProcessSpec{Command: []string{"sh", "-c", "sleep 3600 & echo $!; wait"}})
	var childPID int
	deadline := time.Now().Add(10 * time.Second)
	for childPID == 0 {
		_, _ = o.ReadProcessLog(t.Context(), group, 0, false, func(c guest.LogChunk) error {
			for _, ch := range strings.TrimSpace(string(c.Data)) {
				childPID = childPID*10 + int(ch-'0')
			}
			return nil
		})
		if time.Now().After(deadline) {
			t.Fatal("the group's child never said its pid")
		}
	}
	if err := o.SignalProcess(t.Context(), group, 15); err != nil {
		t.Fatal(err)
	}
	if _, end := follow(t, o, group); end.Exit == nil || end.Exit.Signal != 15 || end.Exit.Code != 143 {
		t.Errorf("a process sent TERM ended %+v", end.Exit)
	}
	waitGone(t, childPID)
	if err := o.SignalProcess(t.Context(), group, 9); err != nil {
		t.Errorf("signalling a process that ended: %v", err)
	}

	for range 3 {
		follow(t, o, startProc(t, o, guest.ProcessSpec{Command: []string{"true"}, Keep: 2}))
	}
	viaShim, err := o.ListProcesses(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	host := o
	host.HostProcRoot = o.ProcRoot
	fromHost, err := host.ListProcesses(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(viaShim) != 2 || len(fromHost) != 2 || viaShim[0].ID != fromHost[0].ID {
		t.Errorf("with 2 finished kept, listed %+v through the shim and %+v from the host", viaShim, fromHost)
	}
	if _, err := o.ReadProcessLog(t.Context(), "p0000000000000000", 0, false, func(guest.LogChunk) error { return nil }); !errors.Is(err, guest.ErrNoProcess) {
		t.Errorf("a process not there: %v", err)
	}
}

// TestASupervisorKilledReadsAsKilled: a process whose supervisor is gone,
// as when its guest stopped, reads as ended by SIGKILL.
func TestASupervisorKilledReadsAsKilled(t *testing.T) {
	o := ops(t)
	id := startProc(t, o, guest.ProcessSpec{Command: []string{"sleep", "3600"}})
	// The supervisor is the lock's holder: kill every process holding it.
	out, _ := exec.Command("fuser", "-k", "-KILL", filepath.Join(o.ProcRoot, id, "lock")).CombinedOutput()
	_ = out
	_ = o.SignalProcess(t.Context(), id, 9)
	deadline := time.Now().Add(10 * time.Second)
	for {
		list, err := o.ListProcesses(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(list) == 1 && list[0].Exit != nil {
			if list[0].Exit.Signal != 9 {
				t.Errorf("ended %+v", list[0].Exit)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("listed %+v", list)
		}
		<-time.After(20 * time.Millisecond)
	}
}
