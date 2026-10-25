package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iamv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/cli"
	"github.com/YukinaMochizuki/tower/internal/client"
)

// world is an apiserver and the environment a CLI runs in.
type world struct {
	t   *testing.T
	env *apiservertest.Env
	// vars are the CLI's environment.
	vars map[string]string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	env := apiservertest.Start(t, func(c *apiserver.Config) { c.DisableControllers = true })
	return &world{t: t, env: env, vars: map[string]string{cli.EnvServer: env.URL}}
}

// asAdmin makes the CLI call with a new admin token's secret.
func (w *world) asAdmin() {
	w.vars[cli.EnvToken] = w.env.CreateToken("cli-admin", iamv1alpha1.Role_ROLE_ADMIN, "")
}

type result struct {
	code           int
	stdout, stderr string
}

func (w *world) run(stdin string, args ...string) result {
	w.t.Helper()
	var out, errOut bytes.Buffer
	code := cli.Run(context.Background(), args, cli.IO{
		In: strings.NewReader(stdin), Out: &out, Err: &errOut, Env: func(k string) string { return w.vars[k] },
	}, cli.Options{})
	return result{code, out.String(), errOut.String()}
}

func (w *world) must(stdin string, args ...string) string {
	w.t.Helper()
	r := w.run(stdin, args...)
	if r.code != cli.ExitOK {
		w.t.Fatalf("tower %s: exit %d: %s", strings.Join(args, " "), r.code, r.stderr)
	}
	return r.stdout
}

const manifests = `apiVersion: tenancy.tower.hangarlab.cloud/v1alpha1
kind: Project
metadata: {name: web}
---
apiVersion: targets.tower.hangarlab.cloud/v1alpha1
kind: Target
metadata: {name: main}
spec: {hostd: {}}
---
apiVersion: environments.tower.hangarlab.cloud/v1alpha1
kind: Repository
metadata: {name: api}
spec: {url: "https://git.example.com/api.git", defaultBranch: main}
`

// TestApplyGetDelete: several documents applied, read back in every
// output, and deleted; what is not there exits ExitNotFound.
func TestApplyGetDelete(t *testing.T) {
	w := newWorld(t)
	w.asAdmin()
	out := w.must(manifests, "apply", "-f", "-", "-p", "web")
	for _, want := range []string{"project/web created", "target/main created", "repository/api created"} {
		if !strings.Contains(out, want) {
			t.Errorf("apply said %q, want %q", out, want)
		}
	}
	if out := w.must(manifests, "apply", "-f", "-", "-p", "web"); !strings.Contains(out, "repository/api applied") {
		t.Errorf("applying again said %q", out)
	}

	var repo map[string]any
	if err := json.Unmarshal([]byte(w.must("", "get", "repository", "api", "-p", "web", "-o", "json")), &repo); err != nil {
		t.Fatal(err)
	}
	if repo["spec"].(map[string]any)["url"] != "https://git.example.com/api.git" {
		t.Errorf("get -o json: %v", repo)
	}
	if out := w.must("", "get", "targets", "-o", "yaml"); !strings.Contains(out, "items:") || !strings.Contains(out, "name: main") {
		t.Errorf("get -o yaml: %s", out)
	}
	if out := w.must("", "get", "repositories", "-p", "web"); !strings.HasPrefix(out, "NAME") || !strings.Contains(out, "api") {
		t.Errorf("get as a table: %s", out)
	}
	w.vars[cli.EnvProject] = "web"
	if out := w.must("", "get", "repository", "-o", "name"); out != "repository/api\n" {
		t.Errorf("get with the project from the environment: %q", out)
	}

	for _, tt := range []struct {
		args []string
		code int
	}{
		{[]string{"get", "repository", "none"}, cli.ExitNotFound},
		{[]string{"get", "gadgets"}, cli.ExitUsage},
		{[]string{"get", "target", "main", "-p", "web"}, cli.ExitUsage},
		{[]string{"get", "target", "-o", "xml"}, cli.ExitUsage},
		{[]string{"apply", "-f", "-", "--create-only"}, cli.ExitConflict},
		{[]string{"frobnicate"}, cli.ExitUsage},
	} {
		stdin := ""
		if tt.args[0] == "apply" {
			stdin = manifests
		}
		if r := w.run(stdin, tt.args...); r.code != tt.code {
			t.Errorf("tower %s: exit %d, want %d: %s", strings.Join(tt.args, " "), r.code, tt.code, r.stderr)
		}
	}

	if out := w.must("", "delete", "repository", "api"); out != "repository/api deleted\n" {
		t.Errorf("delete said %q", out)
	}
	if r := w.run("", "get", "repository", "api"); r.code != cli.ExitNotFound {
		t.Errorf("a deleted repository: exit %d", r.code)
	}
}

// TestNoServerOrTokenIsAUsageError: without a server the CLI calls
// nothing; a token file others can read is refused.
func TestNoServerOrTokenIsAUsageError(t *testing.T) {
	w := newWorld(t)
	delete(w.vars, cli.EnvServer)
	if r := w.run("", "get", "targets"); r.code != cli.ExitUsage || !strings.Contains(r.stderr, cli.EnvServer) {
		t.Errorf("no server: %+v", r)
	}
	w.vars[cli.EnvServer] = w.env.URL
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(w.env.CreateToken("a", iamv1alpha1.Role_ROLE_ADMIN, "")), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := w.run("", "--token-file", path, "get", "targets"); r.code != cli.ExitUsage || !strings.Contains(r.stderr, "chmod 600") {
		t.Errorf("a token file others read: %+v", r)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	w.must("", "--token-file", path, "get", "targets")
}

// TestWait: wait returns once the object is observed, its condition is
// met, or it is gone, and exits ExitTimeout when it is not in time.
func TestWait(t *testing.T) {
	w := newWorld(t)
	w.asAdmin()
	w.must(manifests, "apply", "-f", "-", "-p", "web")
	if r := w.run("", "wait", "target", "main", "--for", "condition=Ready", "--timeout", "200ms"); r.code != cli.ExitTimeout {
		t.Errorf("a condition never met: exit %d: %s", r.code, r.stderr)
	}
	if r := w.run("", "wait", "target", "main", "--for", "observed", "--timeout", "200ms"); r.code != cli.ExitTimeout {
		t.Errorf("a status never written: exit %d: %s", r.code, r.stderr)
	}
	if r := w.run("", "wait", "target", "main", "--for", "nothing"); r.code != cli.ExitUsage {
		t.Errorf("--for nothing: exit %d", r.code)
	}

	done := make(chan result, 1)
	go func() {
		done <- w.run("", "wait", "repository", "api", "-p", "web", "--for", "delete", "--timeout", "30s")
	}()
	if _, err := w.env.AsSystem().Delete(t.Context(), registry.Repository, client.Key{Project: "web", Name: "api"}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.code != cli.ExitOK {
			t.Errorf("waiting for a delete: %+v", r)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("wait --for delete never returned")
	}
}

// TestTokens: the admin socket creates the first admin token; create
// prints a secret once, alone on stdout or in a new private file; rotate
// replaces it, and the old one stops working.
func TestTokens(t *testing.T) {
	w := newWorld(t)
	w.vars[cli.EnvServer] = "unix://" + w.env.AdminSocket
	secret := strings.TrimSpace(w.must("", "token", "create", "root-made", "--role", "admin"))
	if !strings.HasPrefix(secret, "twr_") || strings.Contains(secret, "\n") {
		t.Fatalf("token create printed %q, want the secret alone", secret)
	}
	w.vars[cli.EnvServer], w.vars[cli.EnvToken] = w.env.URL, secret
	w.must("", "get", "targets")

	file := filepath.Join(t.TempDir(), "hostd.token")
	out := w.must("", "token", "create", "hostd-main", "--role", "hostd", "--target", "main", "--secret-file", file)
	kept, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if s := strings.TrimSpace(string(kept)); !strings.HasPrefix(s, "twr_") || strings.Contains(out, s) || !strings.Contains(out, file) {
		t.Errorf("create with a file printed %q, and the file holds %q", out, kept)
	}
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the secret file: %v, %v", fi, err)
	}
	if r := w.run("", "token", "create", "again", "--role", "admin", "--secret-file", file); r.code != cli.ExitUsage {
		t.Errorf("a secret file that exists: exit %d", r.code)
	}
	if r := w.run("", "token", "create", "x", "--role", "superuser"); r.code != cli.ExitUsage {
		t.Errorf("an unknown role: exit %d", r.code)
	}

	rotated := strings.TrimSpace(w.must("", "token", "rotate", "root-made"))
	if rotated == secret || !strings.HasPrefix(rotated, "twr_") {
		t.Fatalf("rotate printed %q", rotated)
	}
	if r := w.run("", "get", "targets"); r.code == cli.ExitOK {
		t.Error("the rotated-away secret still works")
	}
	w.vars[cli.EnvToken] = rotated
	w.must("", "get", "targets")
}

// TestSecretSet: values come from files and stdin, never the command
// line, and no answer shows them.
func TestSecretSet(t *testing.T) {
	w := newWorld(t)
	w.asAdmin()
	w.must(manifests, "apply", "-f", "-", "-p", "web")
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("from-a-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := w.must("from-stdin", "secret", "set", "aws", "-p", "web", "--from-file", "AWS_ACCESS_KEY_ID="+path, "--from-stdin", "AWS_SECRET_ACCESS_KEY")
	if out != "secret/aws created, with 2 keys\n" {
		t.Errorf("secret set said %q", out)
	}
	data, err := w.env.Server.OpenSecret(auth.WithPrincipal(t.Context(), auth.SystemPrincipal), resource.Key{Project: "web", Name: "aws"})
	if err != nil {
		t.Fatal(err)
	}
	if string(data["AWS_ACCESS_KEY_ID"]) != "from-a-file" || string(data["AWS_SECRET_ACCESS_KEY"]) != "from-stdin" {
		t.Errorf("the secret holds %q", data)
	}
	got := w.must("", "get", "secret", "aws", "-p", "web", "-o", "json")
	if strings.Contains(got, "from-a-file") || strings.Contains(got, "from-stdin") {
		t.Errorf("get shows a secret's value: %s", got)
	}
	if out := w.must("piped", "secret", "set", "piped", "-p", "web", "--from-file", "K=-"); out != "secret/piped created, with 1 keys\n" {
		t.Errorf("--from-file K=- said %q", out)
	}
	if data, err := w.env.Server.OpenSecret(auth.WithPrincipal(t.Context(), auth.SystemPrincipal), resource.Key{Project: "web", Name: "piped"}); err != nil || string(data["K"]) != "piped" {
		t.Errorf("--from-file K=- holds %q, %v", data, err)
	}
	if r := w.run("x", "secret", "set", "twice", "-p", "web", "--from-file", "A=-", "--from-stdin", "B"); r.code != cli.ExitUsage || strings.Contains(r.stderr, "goroutine") {
		t.Errorf("stdin asked for twice: %+v", r)
	}
	if r := w.run("", "secret", "set", "aws", "-p", "web"); r.code != cli.ExitUsage {
		t.Errorf("a secret given no values: exit %d", r.code)
	}
	if r := w.run("", "secret", "set", "aws", "-p", "web", "--from-file", "KEY"); r.code != cli.ExitUsage {
		t.Errorf("--from-file without a path: exit %d", r.code)
	}
	if r := w.run("x", "secret", "set", "aws", "-p", "web", "--from-stdin", "K", "--create-only"); r.code != cli.ExitConflict {
		t.Errorf("--create-only over a secret: exit %d", r.code)
	}
}
