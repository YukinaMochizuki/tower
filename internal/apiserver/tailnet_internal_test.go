package apiserver

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/provider"
)

// vmExec is a VM's SSH as joinVM sees it: it records what it was asked to
// run and what it read on stdin, and answers as told.
type vmExec struct {
	provider.Provider
	req          provider.ExecRequest
	stdin        string
	code         int
	out          string
	execErr      error
	echoTheStdin bool
}

func (v *vmExec) Exec(_ context.Context, req provider.ExecRequest) (int, error) {
	v.req = req
	if req.Stdin != nil {
		b, _ := io.ReadAll(req.Stdin)
		v.stdin = string(b)
	}
	out := v.out
	if v.echoTheStdin {
		out += strings.TrimSpace(v.stdin)
	}
	_, _ = io.WriteString(req.Stdout, out)
	return v.code, v.execErr
}

// TestAVMJoinsWithItsKeyOnStdin: the key reaches the VM on the join's
// stdin alone, never in its command, which names the pinned tailscale,
// the hostname and the routes; how it went maps onto TailnetJoined's
// reasons, and a join that failed on the way is an error to try again;
// neither quotes the key, even where tailscale did.
func TestAVMJoinsWithItsKeyOnStdin(t *testing.T) {
	t.Parallel()
	const key = "tskey-auth-kVM-SECRETSECRET"
	ref := provider.Instance{Project: "web", Name: "mc-k3s"}
	tn := &envv1alpha1.Tailnet{AuthSecret: "ts", Hostname: "k3s-1", AcceptRoutes: true}
	for _, tt := range []struct {
		name   string
		v      *vmExec
		reason string
		retry  bool
	}{
		{"joined", &vmExec{out: "joined as k3s-1\n"}, provider.TailnetJoined, false},
		{"refused", &vmExec{code: exitTailnetUp, out: "backend error: invalid key: ", echoTheStdin: true}, provider.TailnetKeyRejected, false},
		{"stopped node", &vmExec{code: exitTailnetState, out: "its node has state, and tailscaled says it is Stopped"}, provider.TailnetFailed, false},
		{"up failed", &vmExec{code: exitTailnetUp, out: "context deadline exceeded ", echoTheStdin: true}, "", true},
		{"download failed", &vmExec{code: 12, out: "curl: (6) Could not resolve host"}, "", true},
		{"not ready", &vmExec{code: 12, out: "tailscaled is not ready: Starting"}, "", true},
		{"no SSH", &vmExec{execErr: errors.New("ssh: " + key + ": " + provider.ErrUnavailable.Error())}, "", true},
	} {
		got, err := joinVM(t.Context(), tt.v, ref, key, tn)
		if tt.retry {
			if err == nil || strings.Contains(err.Error(), key) {
				t.Errorf("%s: %+v, %v; want an error to try again, without the key", tt.name, got, err)
			}
		} else if err != nil || got.Reason != tt.reason {
			t.Errorf("%s: %+v, %v; want %s", tt.name, got, err, tt.reason)
		}
		if strings.Contains(got.Message, key) {
			t.Errorf("%s: its message quotes the key: %q", tt.name, got.Message)
		}
		if strings.TrimSpace(tt.v.stdin) != key {
			t.Errorf("%s: the VM read %q on stdin", tt.name, tt.v.stdin)
		}
		cmd := tt.v.req.Command
		if slices.ContainsFunc(cmd, func(a string) bool { return strings.Contains(a, key) }) || len(tt.v.req.Env) > 0 || tt.v.req.AsRoot {
			t.Errorf("%s: the key, an environment or AsRoot reached the command: %q %v", tt.name, cmd, tt.v.req.Env)
		}
		if want := []string{"sh", "-c", joinVMTailnet, "tower-tailnet", tailscaleVersion, tailscaleSHA256, "k3s-1", "1", vmUnits}; !slices.Equal(cmd, want) {
			t.Errorf("%s: ran %q", tt.name, cmd[3:])
		}
	}
}

// fakeVM is a VM's commands for its scripts, in a directory first on
// PATH: tailscale, whose status says the BackendState its state file
// holds and whose up records its arguments and stdin and refuses keys
// starting "tskey-rejected"; tailscaled, there; systemctl and sleep, which
// record and do nothing.
type fakeVM struct {
	bin, log, state string
}

func newFakeVM(t *testing.T, backend string) *fakeVM {
	t.Helper()
	dir := t.TempDir()
	v := &fakeVM{bin: filepath.Join(dir, "bin"), log: filepath.Join(dir, "log"), state: filepath.Join(dir, "backend")}
	if err := os.MkdirAll(v.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(v.bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("tailscale", `echo "tailscale $*" >>`+v.log+`
case $1 in
status) printf '{\n  "BackendState": "%s"\n}\n' "$(cat `+v.state+`)" ;;
up) key=$(cat /dev/stdin); echo "up-stdin $key" >>`+v.log+`
	case $key in tskey-rejected*) echo "backend error: invalid key: $key not valid" >&2; exit 1 ;; esac
	echo Running >`+v.state+` ;;
esac
`)
	write("tailscaled", "exit 0\n")
	write("systemctl", `echo "systemctl $*" >>`+v.log+"\n")
	write("sleep", "exit 0\n")
	if err := os.WriteFile(v.state, []byte(backend), 0o644); err != nil {
		t.Fatal(err)
	}
	return v
}

// run runs a script as the VM's ssh would, stdin given.
func (v *fakeVM) run(t *testing.T, stdin, script string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"-c", script, "tower-tailnet"}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+v.bin+":"+os.Getenv("PATH"))
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatal(err)
	}
	return cmd.ProcessState.ExitCode(), string(out)
}

func (v *fakeVM) logged() string {
	b, _ := os.ReadFile(v.log)
	return string(b)
}

// TestTheVMJoinScript: a node with no state is joined with the key from
// stdin, given to tailscale up on its stdin alone, without accepting
// DNS, and a drop-in routes ts.net alone to MagicDNS; a node with state
// is left as it is; a refused key exits as tailscale up failing; a
// tailscaled not ready is an exit to try again.
func TestTheVMJoinScript(t *testing.T) {
	t.Parallel()
	const key = "tskey-auth-kScript-Zz11"
	v := newFakeVM(t, "NeedsLogin")
	units := t.TempDir()
	code, out := v.run(t, key+"\n", joinVMTailnet, "1.0", "sum", "k3s-1", "1", units)
	logged := v.logged()
	if code != 0 || !strings.Contains(logged, "up-stdin "+key) {
		t.Fatalf("joined: exit %d: %s; log %s", code, out, logged)
	}
	for line := range strings.Lines(logged) {
		if strings.Contains(line, key) && !strings.HasPrefix(line, "up-stdin ") {
			t.Errorf("the key reached a command's arguments: %q", line)
		}
	}
	for _, want := range []string{"--auth-key=file:/dev/stdin", "--hostname=k3s-1", "--accept-dns=false", "--accept-routes"} {
		if !strings.Contains(logged, want) {
			t.Errorf("tailscale up was not given %s: %s", want, logged)
		}
	}
	dropIn, err := os.ReadFile(filepath.Join(units, "tailscaled.service.d", "tower-dns.conf"))
	if err != nil || !strings.Contains(string(dropIn), `"~ts.net"`) {
		t.Errorf("the DNS split: %q, %v", dropIn, err)
	}
	// On a VM without systemd-resolved, the split is skipped at once.
	_, line, _ := strings.Cut(string(dropIn), "ExecStartPost=-/bin/sh -c '")
	line = strings.ReplaceAll(strings.TrimSuffix(strings.TrimSpace(line), "'"), "$$", "$")
	start := time.Now()
	bare := t.TempDir()
	// A sleep that waits, so retries a guard missed would show.
	if err := os.WriteFile(filepath.Join(bare, "sleep"), []byte("#!/bin/sh\nexec /bin/sleep 0.2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", line)
	cmd.Env = []string{"PATH=" + bare}
	if out, err := cmd.CombinedOutput(); err != nil || time.Since(start) > 2*time.Second {
		t.Errorf("the split on a VM without resolvectl: %v after %v: %s", err, time.Since(start), out)
	}

	again := newFakeVM(t, "Running")
	if code, out := again.run(t, key, joinVMTailnet, "1.0", "sum", "k3s-1", "", units); code != 0 || strings.Contains(again.logged(), "tailscale up") {
		t.Errorf("a node with state: exit %d: %s; log %s", code, out, again.logged())
	}
	refused := newFakeVM(t, "NeedsLogin")
	if code, _ := refused.run(t, "tskey-rejected-x", joinVMTailnet, "1.0", "sum", "k3s-1", "", units); code != exitTailnetUp {
		t.Errorf("a refused key exited %d", code)
	}
	starting := newFakeVM(t, "Starting")
	if code, _ := starting.run(t, key, joinVMTailnet, "1.0", "sum", "k3s-1", "", units); code == 0 || code == exitTailnetUp || code == exitTailnetState {
		t.Errorf("a tailscaled not ready exited %d; want one to try again", code)
	}
}

// TestTheVMLeaveScript: leaving, the node logs out, tailscaled stops and
// is disabled, and its state and tower's DNS split go.
func TestTheVMLeaveScript(t *testing.T) {
	t.Parallel()
	v := newFakeVM(t, "Running")
	state, units := filepath.Join(t.TempDir(), "tailscale"), t.TempDir()
	for _, f := range []string{filepath.Join(state, "tailscaled.state"), filepath.Join(units, "tailscaled.service.d", "tower-dns.conf")} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if code, out := v.run(t, "", leaveVMTailnet, state, units); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	logged := v.logged()
	if !strings.Contains(logged, "tailscale logout") || !strings.Contains(logged, "systemctl disable --now tailscaled.service") {
		t.Errorf("it did not log out and stop tailscaled: %s", logged)
	}
	for _, f := range []string{state, filepath.Join(units, "tailscaled.service.d", "tower-dns.conf")} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still there", f)
		}
	}
}

// TestTheGoldenScrub: a golden's builder keeps no tailscale node state.
func TestTheGoldenScrub(t *testing.T) {
	t.Parallel()
	v := newFakeVM(t, "Running")
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "tailscaled.state"), []byte("node"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := v.run(t, "", scrubGoldenTailnet, state); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(state, "tailscaled.state")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the golden keeps a node's state")
	}
}

// TestTheVMScriptsAreShell: the scripts parse as sh.
func TestTheVMScriptsAreShell(t *testing.T) {
	t.Parallel()
	for name, script := range map[string]string{"join": joinVMTailnet, "scrub": scrubGoldenTailnet, "leave": leaveVMTailnet, "set": setVMTailnet, "state": vmBackendState} {
		if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("%s: %v: %s", name, err, out)
		}
	}
}

// TestTheMachinesTailscaleIsTheHostsPinned: what tower installs in a
// machine is what deploy/hostd/versions.env pins for every tailscale tower
// runs.
func TestTheMachinesTailscaleIsTheHostsPinned(t *testing.T) {
	t.Parallel()
	_, here, _, _ := runtime.Caller(0)
	f, err := os.Open(filepath.Join(filepath.Dir(here), "..", "..", "deploy", "hostd", "versions.env"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	pinned := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			pinned[k] = strings.Trim(v, `"`)
		}
	}
	if pinned["TOWER_TAILSCALE_VERSION"] != tailscaleVersion || pinned["TOWER_TAILSCALE_SHA256"] != tailscaleSHA256 {
		t.Errorf("versions.env pins %s %s; tower installs %s %s", pinned["TOWER_TAILSCALE_VERSION"], pinned["TOWER_TAILSCALE_SHA256"], tailscaleVersion, tailscaleSHA256)
	}
}
