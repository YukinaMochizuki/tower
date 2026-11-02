package apiserver_test

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	secretsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/secrets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// The values a privileged box is given in TestACopyOutIsScannedForTheSourcesSecrets.
const (
	// copyToken's base64 has + and /, so its base64url differs. Its prefix
	// is split so GitHub push protection does not take it for a real token.
	copyToken = "ghp_" + "Wq07/canary+Token=Lp30~~~xyz???ABC"
	// copyPassword is short, and its file ends in a newline.
	copyPassword   = "Pw-Zq8/1x\n"
	copyKubeToken  = "eyJhbGciOiJSUzI1NiJ9.canaryKubeToken0123456789abcdefXYZ"
	kubeStructure  = "apiVersion: v1\nkind: Config\nclusters:\n- cluster:\n    server: https://kube1.example.com:6443\n  name: prod\ncontexts:\n- context:\n    cluster: prod\n    user: ci\n  name: prod\ncurrent-context: prod\n"
	copyKubeconfig = kubeStructure + "users:\n- name: ci\n  user:\n    token: " + copyKubeToken + "\n"
	kubeconfigPath = "/home/dev/.kube/config"
	// copyConf is a second secret of several parts, its key a piece.
	copyConfKey  = "ghp_" + "C0nfApiKeyValue77AbCdXy"
	copyConf     = "endpoint: https://api.example.com\napi_key: " + copyConfKey + "\n"
	confPath     = "/home/dev/.config/tool.yaml"
	passwordPath = "/home/dev/.pw"
)

// refusalOf is the CopyRefusal detail of err, nil for none.
func refusalOf(err error) *envv1alpha1.CopyRefusal {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return nil
	}
	for _, d := range ce.Details() {
		if v, err := d.Value(); err == nil {
			if r, ok := v.(*envv1alpha1.CopyRefusal); ok {
				return r
			}
		}
	}
	return nil
}

// wrap is s broken into lines of n.
func wrap(s string, n int) string {
	var b strings.Builder
	for len(s) > n {
		b.WriteString(s[:n] + "\n")
		s = s[n:]
	}
	b.WriteString(s)
	return b.String()
}

// TestACopyOutIsScannedForTheSourcesSecrets: a copy asked to refuse
// secrets streams through tower, and is refused, naming the secret by its
// variable or path and the form it was found in, when what it copies holds
// a value the source was given: raw, trimmed, a line of a multi-line one,
// or encoded. A refused copy leaves nothing at the destination, nor beside
// it, and no value reaches the error, an object, an event or the log.
func TestACopyOutIsScannedForTheSourcesSecrets(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	w := newWorld(t, worldOption{config: func(c *apiserver.Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}})
	w.mustApply(registry.Secret, secret("web", "forge", map[string]string{"GH_TOKEN": copyToken}))
	w.mustApply(registry.Secret, secret("web", "files", map[string]string{"kube": copyKubeconfig, "pw": copyPassword, "conf": copyConf}))
	w.readyBox("priv", func(b *envv1alpha1.Box) {
		b.Spec.PrivilegedSecrets = []*envv1alpha1.SecretRef{{Secret: "forge", Key: "GH_TOKEN", As: "GH_TOKEN"}}
		b.Spec.SecretFiles = []*envv1alpha1.SecretFile{
			{Secret: "files", Key: "kube", Path: kubeconfigPath},
			{Secret: "files", Key: "pw", Path: passwordPath},
			{Secret: "files", Key: "conf", Path: confPath},
		}
	})
	dst := w.readyBox("plain")
	dstRef := provider.Instance{Project: "web", Name: dst.GetStatus().GetInstance()}
	envs := w.as(workstream).envs
	ctx := t.Context()
	copyOut := func(from, to string) error {
		_, err := envs.CopyFile(ctx, connect.NewRequest(&envv1alpha1.CopyFileRequest{
			From: boxEnv("priv"), FromPath: from, To: boxEnv("plain"), ToPath: to, RefuseSecrets: true,
		}))
		return err
	}
	nothingAt := func(t *testing.T, to string) {
		t.Helper()
		for name := range w.fake.Files(dstRef) {
			if strings.HasPrefix(name, to) {
				t.Errorf("a refused copy left %s", name)
			}
		}
	}

	t.Run("clean", func(t *testing.T) {
		if err := writeFile(ctx, envs, boxEnv("priv"), "/tmp/out/report.md", "# 3 findings\n", 0); err != nil {
			t.Fatal(err)
		}
		if err := writeFile(ctx, envs, boxEnv("priv"), "/tmp/out/data/list.json", `{"cves":[]}`, 0); err != nil {
			t.Fatal(err)
		}
		if err := copyOut("/tmp/out", "/tmp/in"); err != nil {
			t.Fatal(err)
		}
		files := w.fake.Files(dstRef)
		if files["/tmp/in/report.md"] != "# 3 findings\n" || files["/tmp/in/data/list.json"] != `{"cves":[]}` {
			t.Errorf("the copy reached plain as %v", files)
		}
		if err := copyOut("/tmp/out/report.md", "/tmp/report.md"); err != nil || w.fake.Files(dstRef)["/tmp/report.md"] != "# 3 findings\n" {
			t.Errorf("a file's copy: %v, %q", err, w.fake.Files(dstRef)["/tmp/report.md"])
		}
		for name := range w.fake.Files(dstRef) {
			if strings.Contains(name, ".part") {
				t.Errorf("a copy left %s", name)
			}
		}
	})

	t.Run("a kubeconfig's structure", func(t *testing.T) {
		if err := writeFile(ctx, envs, boxEnv("priv"), "/tmp/out/kubeconfig.yaml", kubeStructure, 0); err != nil {
			t.Fatal(err)
		}
		if err := copyOut("/tmp/out/kubeconfig.yaml", "/tmp/kubeconfig.yaml"); err != nil {
			t.Errorf("a copy of a kubeconfig's structural lines: %v", err)
		}
	})

	const found, suspect = envv1alpha1.CopyRefusalReason_COPY_REFUSAL_REASON_SECRET_FOUND, envv1alpha1.CopyRefusalReason_COPY_REFUSAL_REASON_SUSPECT
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	for _, tt := range []struct {
		name, content, secret, form string
		reason                      envv1alpha1.CopyRefusalReason
	}{
		{"raw", "export GH_TOKEN=" + copyToken + "\n", "$GH_TOKEN", "raw", found},
		{"hex", strings.ToUpper(hex.EncodeToString([]byte(copyToken))), "$GH_TOKEN", "hex", found},
		{"base64", b64("x" + copyToken + "y"), "$GH_TOKEN", "base64", found},
		{"base64, two bytes in", b64("xy" + copyToken), "$GH_TOKEN", "base64", found},
		{"base64url", base64.RawURLEncoding.EncodeToString([]byte(copyToken)), "$GH_TOKEN", "base64url", found},
		{"wrapped base64", wrap(b64("token: "+copyToken+"\nmore text after it"), 19) + "\n", "$GH_TOKEN", "base64", found},
		{"url-encoded", "https://x.example/cb?t=" + url.QueryEscape(copyToken), "$GH_TOKEN", "url", found},
		{"json-escaped", `{"t":"` + strings.ReplaceAll(copyToken, "/", `\/`) + `"}`, "$GH_TOKEN", "json", found},
		{"short secret file in json", `{"password":"Pw-Zq8\/1x"}`, passwordPath, "json", found},
		{"short secret file trimmed", "password=Pw-Zq8/1x;", passwordPath, "raw", found},
		{"a kubeconfig's token line", "TOKEN=" + copyKubeToken, kubeconfigPath, "raw", suspect},
		{"a kubeconfig's token base64", b64("t=" + copyKubeToken), kubeconfigPath, "base64", suspect},
		{"a whole kubeconfig", "# saved\n" + copyKubeconfig, kubeconfigPath, "raw", found},
	} {
		t.Run(tt.name, func(t *testing.T) {
			from := "/tmp/leak/" + strings.ReplaceAll(tt.name, " ", "-")
			if err := writeFile(ctx, envs, boxEnv("priv"), from, tt.content, 0); err != nil {
				t.Fatal(err)
			}
			to := "/tmp/got/" + strings.ReplaceAll(tt.name, " ", "-")
			err := copyOut(from, to)
			wantCode(t, err, connect.CodeFailedPrecondition, "a copy holding "+tt.secret)
			if prefix := "copy refused: " + strings.TrimPrefix(tt.reason.String(), "COPY_REFUSAL_REASON_") + ": "; !strings.HasPrefix(messageOfErr(err), prefix) {
				t.Errorf("the refusal says %q; want it to start %q", messageOfErr(err), prefix)
			}
			if r := refusalOf(err); r.GetReason() != tt.reason || !slices.Equal(r.GetSecrets(), []string{tt.secret}) || r.GetForm() != tt.form {
				t.Errorf("the refusal's detail is %v; want %s, %s found %s", r, tt.reason, tt.secret, tt.form)
			}
			nothingAt(t, to)
			noValueIn(t, err)
		})
	}

	t.Run("allow_suspect_for", func(t *testing.T) {
		allowed := func(from, to string, names ...string) error {
			_, err := envs.CopyFile(ctx, connect.NewRequest(&envv1alpha1.CopyFileRequest{
				From: boxEnv("priv"), FromPath: from, To: boxEnv("plain"), ToPath: to, RefuseSecrets: true, AllowSuspectFor: names,
			}))
			return err
		}
		piece := "TOKEN=" + copyKubeToken + "\n"
		if err := writeFile(ctx, envs, boxEnv("priv"), "/tmp/approved/piece", piece, 0); err != nil {
			t.Fatal(err)
		}
		if err := allowed("/tmp/approved/piece", "/tmp/approved-piece", kubeconfigPath); err != nil || w.fake.Files(dstRef)["/tmp/approved-piece"] != piece {
			t.Errorf("a piece of the secret allowed: %v, %q", err, w.fake.Files(dstRef)["/tmp/approved-piece"])
		}
		// Another secret's piece is still SUSPECT, even beside the allowed one.
		both := piece + "key=" + copyConfKey + "\n"
		if err := writeFile(ctx, envs, boxEnv("priv"), "/tmp/approved/both", both, 0); err != nil {
			t.Fatal(err)
		}
		for _, tt := range []struct {
			allowed []string
			named   string
		}{
			{[]string{kubeconfigPath}, confPath},
			{[]string{"$GH_TOKEN", "/home/dev/.other"}, kubeconfigPath},
		} {
			err := allowed("/tmp/approved/both", "/tmp/approved-both", tt.allowed...)
			if r := refusalOf(err); r.GetReason() != suspect || !slices.Equal(r.GetSecrets(), []string{tt.named}) {
				t.Errorf("%v allowed: %v; want SUSPECT of %s", tt.allowed, err, tt.named)
			}
			nothingAt(t, "/tmp/approved-both")
		}
		if err := allowed("/tmp/approved/both", "/tmp/approved-both", confPath, kubeconfigPath); err != nil {
			t.Errorf("both secrets' pieces allowed: %v", err)
		}
		for _, tt := range []struct{ name, content, secret string }{
			{"a whole kubeconfig", "# saved\n" + copyKubeconfig, kubeconfigPath},
			{"an env secret", "t=" + copyToken, "$GH_TOKEN"},
			{"a short secret file", `{"password":"Pw-Zq8\/1x"}`, passwordPath},
		} {
			from := "/tmp/approved/" + strings.ReplaceAll(tt.name, " ", "-")
			if err := writeFile(ctx, envs, boxEnv("priv"), from, tt.content, 0); err != nil {
				t.Fatal(err)
			}
			to := "/tmp/approved-" + strings.ReplaceAll(tt.name, " ", "-")
			err := allowed(from, to, kubeconfigPath, passwordPath, "$GH_TOKEN", confPath)
			if r := refusalOf(err); r.GetReason() != found || !slices.Equal(r.GetSecrets(), []string{tt.secret}) ||
				!strings.HasPrefix(messageOfErr(err), "copy refused: SECRET_FOUND: ") {
				t.Errorf("%s, allow_suspect: %v, %v; want SECRET_FOUND of %s", tt.name, err, r, tt.secret)
			}
			nothingAt(t, to)
			noValueIn(t, err)
		}
	})

	t.Run("directories", func(t *testing.T) {
		// A secret file's path is a link to its value in memory: tar copies
		// the link, which holds no value.
		if err := copyOut("/home/dev", "/tmp/home"); err != nil {
			t.Fatalf("a copy of the directory a secret file's link is in: %v", err)
		}
		if links := w.fake.Links(dstRef); !strings.HasPrefix(links["/tmp/home/.kube/config"], provider.SecretsDir+"/") {
			t.Errorf("the link reached plain as %v", links)
		}
		for name, content := range w.fake.Files(dstRef) {
			if strings.HasPrefix(name, "/tmp/home") && strings.Contains(content, copyKubeToken) {
				t.Errorf("%s holds a secret", name)
			}
		}
		if err := writeFile(ctx, envs, boxEnv("priv"), "/tmp/dirleak/sub/notes.txt", "saw "+copyToken, 0); err != nil {
			t.Fatal(err)
		}
		for _, from := range []string{provider.SecretsDir, "/tmp/dirleak"} {
			err := copyOut(from, "/tmp/dir-got")
			if r := refusalOf(err); r.GetReason() != envv1alpha1.CopyRefusalReason_COPY_REFUSAL_REASON_SECRET_FOUND {
				t.Errorf("a copy of %s: %v", from, err)
			}
			nothingAt(t, "/tmp/dir-got")
			noValueIn(t, err)
		}
	})

	t.Run("not there", func(t *testing.T) {
		wantCode(t, copyOut("/tmp/none", "/tmp/none"), connect.CodeNotFound, "a copy of a path not there")
		nothingAt(t, "/tmp/none")
	})

	if !strings.Contains(logs.String(), "refused=SECRET_FOUND") {
		t.Fatalf("the log was not captured:\n%s", logs.String())
	}
	w.waitSettled()
	for _, v := range []string{copyToken, strings.TrimSpace(copyPassword), copyKubeToken, copyConfKey} {
		w.nowhere(v, &logs)
	}
}

// noValueIn fails the test if err says any of the values
// TestACopyOutIsScannedForTheSourcesSecrets gives its box.
func noValueIn(t *testing.T, err error) {
	t.Helper()
	text := err.Error()
	if r := refusalOf(err); r != nil {
		text += r.String()
	}
	for _, v := range []string{copyToken, strings.TrimSpace(copyPassword), copyKubeToken, copyConfKey} {
		if strings.Contains(text, v) {
			t.Errorf("the refusal holds a secret's value: %s", text)
		}
	}
}

// TestACopyPastItsMaxIsRefused: a copy with max_bytes is refused once it
// streamed more, leaving nothing; one within it goes.
func TestACopyPastItsMaxIsRefused(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.readyBox("a")
	dst := w.readyBox("b")
	envs := w.as(workstream).envs
	ctx := t.Context()
	if err := writeFile(ctx, envs, boxEnv("a"), "/tmp/big", strings.Repeat("x", 8<<10), 0); err != nil {
		t.Fatal(err)
	}
	copyFile := func(max int64) error {
		_, err := envs.CopyFile(ctx, connect.NewRequest(&envv1alpha1.CopyFileRequest{From: boxEnv("a"), FromPath: "/tmp/big", To: boxEnv("b"), ToPath: "/tmp/big", MaxBytes: max}))
		return err
	}
	err := copyFile(4 << 10)
	if r := refusalOf(err); r.GetReason() != envv1alpha1.CopyRefusalReason_COPY_REFUSAL_REASON_TOO_LARGE || r.GetMaxBytes() != 4<<10 ||
		!strings.HasPrefix(messageOfErr(err), "copy refused: TOO_LARGE: ") || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("a copy past its max: %v, %v", err, r)
	}
	ref := provider.Instance{Project: "web", Name: dst.GetStatus().GetInstance()}
	for name := range w.fake.Files(ref) {
		if strings.HasPrefix(name, "/tmp/big") {
			t.Errorf("a refused copy left %s", name)
		}
	}
	if err := copyFile(64 << 10); err != nil || len(w.fake.Files(ref)["/tmp/big"]) != 8<<10 {
		t.Errorf("a copy within its max: %v", err)
	}
	wantCode(t, copyFile(-1), connect.CodeInvalidArgument, "a negative max_bytes")
}

// TestACopyOutOfABoxWithAShortSecretIsRefusedUnread: a box given a secret
// too short to scan for is refused, naming it, before anything is read.
func TestACopyOutOfABoxWithAShortSecretIsRefusedUnread(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.mustApply(registry.Secret, secret("web", "pin", map[string]string{"pin": "4711"}))
	src := w.readyBox("priv", func(b *envv1alpha1.Box) {
		b.Spec.PrivilegedSecrets = []*envv1alpha1.SecretRef{{Secret: "pin", Key: "pin", As: "PIN"}}
	})
	w.readyBox("plain")
	envs := w.as(workstream).envs
	_, err := envs.CopyFile(t.Context(), connect.NewRequest(&envv1alpha1.CopyFileRequest{
		From: boxEnv("priv"), FromPath: "/tmp/out", To: boxEnv("plain"), ToPath: "/tmp/in", RefuseSecrets: true,
	}))
	r := refusalOf(err)
	if r.GetReason() != envv1alpha1.CopyRefusalReason_COPY_REFUSAL_REASON_UNSCANNABLE || !slices.Equal(r.GetSecrets(), []string{"$PIN"}) ||
		!strings.HasPrefix(messageOfErr(err), "copy refused: UNSCANNABLE: ") || !strings.Contains(messageOfErr(err), "$PIN") {
		t.Errorf("a copy out of a box with a short secret: %v, %v", err, r)
	}
	if strings.Contains(err.Error(), "4711") {
		t.Errorf("the refusal holds the value: %v", err)
	}
	srcRef := provider.Instance{Project: "web", Name: src.GetStatus().GetInstance()}
	for _, e := range w.fake.Execs() {
		if e.Instance == srcRef && len(e.Command) > 0 && e.Command[0] == "tar" {
			t.Errorf("the source was read: %v", e.Command)
		}
	}
}

// TestCapabilitiesNameTheCopyScan: workstream, and it alone, learns that
// this tower scans copies, as it must before it asks one to.
func TestCapabilitiesNameTheCopyScan(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	res, err := w.as(workstream).envs.GetCapabilities(t.Context(), connect.NewRequest(&envv1alpha1.GetCapabilitiesRequest{}))
	if err != nil || !slices.Contains(res.Msg.GetCapabilities(), "copy.refuse_secrets.v1") {
		t.Errorf("capabilities: %v, %v", res, err)
	}
	_, err = w.as(admin).envs.GetCapabilities(t.Context(), connect.NewRequest(&envv1alpha1.GetCapabilitiesRequest{}))
	wantCode(t, err, connect.CodePermissionDenied, "an admin's capabilities")
}

// TestAScannedCopySaysSo: a copy that streamed through tower answers
// scanned, so a caller can tell it from one a tower ignoring the options
// made.
func TestAScannedCopySaysSo(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.readyBox("a")
	w.readyBox("b")
	envs := w.as(workstream).envs
	ctx := t.Context()
	if err := writeFile(ctx, envs, boxEnv("a"), "/tmp/out", "built", 0); err != nil {
		t.Fatal(err)
	}
	for _, req := range []*envv1alpha1.CopyFileRequest{
		{From: boxEnv("a"), FromPath: "/tmp/out", To: boxEnv("b"), ToPath: "/tmp/in", RefuseSecrets: true},
		{From: boxEnv("a"), FromPath: "/tmp/out", To: boxEnv("b"), ToPath: "/tmp/in2", MaxBytes: 1 << 20},
	} {
		res, err := envs.CopyFile(ctx, connect.NewRequest(req))
		if err != nil || !res.Msg.GetScanned() {
			t.Errorf("a scanned copy: %v, %v", res, err)
		}
	}
}

// TestACopyWithoutOptionsStaysOnTheTarget: without refuse_secrets or
// max_bytes, a copy is the target's own, never streamed through tower;
// allow_suspect alone does not change that.
func TestACopyWithoutOptionsStaysOnTheTarget(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.readyBox("a")
	dst := w.readyBox("b")
	envs := w.as(workstream).envs
	ctx := t.Context()
	if err := writeFile(ctx, envs, boxEnv("a"), "/tmp/out", "built", 0); err != nil {
		t.Fatal(err)
	}
	for _, req := range []*envv1alpha1.CopyFileRequest{
		{From: boxEnv("a"), FromPath: "/tmp/out", To: boxEnv("b"), ToPath: "/tmp/in"},
		// allow_suspect_for without refuse_secrets does nothing.
		{From: boxEnv("a"), FromPath: "/tmp/out", To: boxEnv("b"), ToPath: "/tmp/in2", AllowSuspectFor: []string{"$GH_TOKEN"}},
	} {
		res, err := envs.CopyFile(ctx, connect.NewRequest(req))
		if err != nil {
			t.Fatal(err)
		}
		if res.Msg.GetScanned() {
			t.Errorf("a copy that stayed on the target says it was scanned")
		}
	}
	if got := w.fake.Files(provider.Instance{Project: "web", Name: dst.GetStatus().GetInstance()})["/tmp/in"]; got != "built" {
		t.Errorf("the copy reached b as %q", got)
	}
	for _, e := range w.fake.Execs() {
		if len(e.Command) > 0 && (e.Command[0] == "tar" || e.Command[0] == "rm") {
			t.Errorf("a copy without options ran %v", e.Command)
		}
	}
}

// TestAFailedCopySaysNoMoreThanItsExit: when tar fails in the source, the
// error says its exit and whether the path is there, never what tar
// wrote, which names paths, and a path may hold a secret.
func TestAFailedCopySaysNoMoreThanItsExit(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	w := newWorld(t, worldOption{config: func(c *apiserver.Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}})
	w.mustApply(registry.Secret, secret("web", "forge", map[string]string{"GH_TOKEN": copyToken}))
	w.readyBox("priv", func(b *envv1alpha1.Box) {
		b.Spec.PrivilegedSecrets = []*envv1alpha1.SecretRef{{Secret: "forge", Key: "GH_TOKEN", As: "GH_TOKEN"}}
	})
	w.readyBox("plain")
	envs := w.as(workstream).envs
	ctx := t.Context()
	copyOut := func(from string) error {
		_, err := envs.CopyFile(ctx, connect.NewRequest(&envv1alpha1.CopyFileRequest{
			From: boxEnv("priv"), FromPath: from, To: boxEnv("plain"), ToPath: "/tmp/in", RefuseSecrets: true,
		}))
		return err
	}
	// tar names what it cannot find, here a secret.
	err := copyOut("/tmp/missing-" + copyToken)
	wantCode(t, err, connect.CodeNotFound, "a copy of a path not there")
	if messageOfErr(err) != "copy failed: tar exited 2: no such file or directory" {
		t.Errorf("the error says %q", messageOfErr(err))
	}
	if err := writeFile(ctx, envs, boxEnv("priv"), "/tmp/out", "built", 0); err != nil {
		t.Fatal(err)
	}
	w.fake.FailCommands("tar")
	err = copyOut("/tmp/out")
	w.fake.FailCommands()
	wantCode(t, err, connect.CodeFailedPrecondition, "a copy whose tar failed")
	if messageOfErr(err) != "copy failed: tar exited 1" {
		t.Errorf("the error says %q", messageOfErr(err))
	}
	if !strings.Contains(logs.String(), "stage=tar exit_code=2") {
		t.Fatalf("the log was not captured:\n%s", logs.String())
	}
	w.waitSettled()
	w.nowhere(copyToken, &logs)
}

// TestARotatedSecretIsStillScannedFor: a copy out of a box is scanned for
// every value its secrets held since the box was made, not only the
// current ones; a value replaced before it was made is not looked for; and
// once a value the box may hold is no longer kept, its copies cannot be
// scanned. The values kept stay out of every answer.
func TestARotatedSecretIsStillScannedFor(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	w := newWorld(t, worldOption{config: func(c *apiserver.Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}})
	const before, old, current = "canary-before-Yt52aa", "canary-old-Hq61bb", "canary-new-Pk20cc"
	use := func(b *envv1alpha1.Box) {
		b.Spec.PrivilegedSecrets = []*envv1alpha1.SecretRef{{Secret: "forge", Key: "GH_TOKEN", As: "GH_TOKEN"}}
	}
	w.mustApply(registry.Secret, secret("web", "forge", map[string]string{"GH_TOKEN": before}))
	w.advance(time.Minute)
	w.mustApply(registry.Secret, secret("web", "forge", map[string]string{"GH_TOKEN": old}))
	w.advance(time.Minute)
	w.readyBox("priv", use)
	w.readyBox("plain")
	w.advance(time.Minute)
	w.mustApply(registry.Secret, secret("web", "forge", map[string]string{"GH_TOKEN": current}))
	envs := w.as(workstream).envs
	ctx := t.Context()
	copyOut := func(box, content string) error {
		if err := writeFile(ctx, envs, boxEnv(box), "/tmp/out", content, 0); err != nil {
			t.Fatal(err)
		}
		_, err := envs.CopyFile(ctx, connect.NewRequest(&envv1alpha1.CopyFileRequest{
			From: boxEnv(box), FromPath: "/tmp/out", To: boxEnv("plain"), ToPath: "/tmp/in", RefuseSecrets: true,
		}))
		return err
	}
	for _, v := range []string{old, current} {
		if r := refusalOf(copyOut("priv", "token="+v)); r.GetReason() != envv1alpha1.CopyRefusalReason_COPY_REFUSAL_REASON_SECRET_FOUND || r.GetSecrets()[0] != "$GH_TOKEN" {
			t.Errorf("a value the box was given: %v", r)
		}
	}
	if err := copyOut("priv", "token="+before); err != nil {
		t.Errorf("a value replaced before the box was made: %v", err)
	}
	got, err := w.as(admin).secrets.GetSecret(ctx, connect.NewRequest(&secretsv1alpha1.GetSecretRequest{Project: "web", Name: "forge"}))
	if err != nil || got.Msg.GetSecret().GetSpec().GetSuperseded() != nil || got.Msg.GetSecret().GetSpec().GetHistorySince() != nil || got.Msg.GetSecret().GetSpec().GetSealed() != nil {
		t.Errorf("an answer holds a secret's history: %v, %v", got.Msg.GetSecret().GetSpec(), err)
	}
	// Past what a secret keeps, the box's oldest value is gone: its
	// copies cannot be cleared, while a box made since can be.
	for i := range 16 {
		w.mustApply(registry.Secret, secret("web", "forge", map[string]string{"GH_TOKEN": fmt.Sprintf("canary-%02d-Rm77dd", i)}))
	}
	err = copyOut("priv", "clean")
	if r := refusalOf(err); r.GetReason() != envv1alpha1.CopyRefusalReason_COPY_REFUSAL_REASON_UNSCANNABLE || !slices.Equal(r.GetSecrets(), []string{"$GH_TOKEN"}) {
		t.Errorf("a box given a value no longer kept: %v", err)
	}
	w.advance(time.Minute)
	w.readyBox("late", use)
	if err := copyOut("late", "clean"); err != nil {
		t.Errorf("a box made since: %v", err)
	}
	w.waitSettled()
	for _, v := range []string{before, old, current} {
		w.nowhere(v, &logs)
	}
}

// TestACopyCrossingItsMaxIsScannedFirst: what the write that passes
// max_bytes holds before it is scanned, and a secret there decides over
// the size.
func TestACopyCrossingItsMaxIsScannedFirst(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.mustApply(registry.Secret, secret("web", "forge", map[string]string{"GH_TOKEN": copyToken}))
	w.readyBox("priv", func(b *envv1alpha1.Box) {
		b.Spec.PrivilegedSecrets = []*envv1alpha1.SecretRef{{Secret: "forge", Key: "GH_TOKEN", As: "GH_TOKEN"}}
	})
	w.readyBox("plain")
	envs := w.as(workstream).envs
	ctx := t.Context()
	content := strings.Repeat("a", 50) + copyToken + strings.Repeat("b", 20000)
	if err := writeFile(ctx, envs, boxEnv("priv"), "/tmp/out", content, 0); err != nil {
		t.Fatal(err)
	}
	// One header, then the secret 50 bytes in: the limit falls just past it.
	_, err := envs.CopyFile(ctx, connect.NewRequest(&envv1alpha1.CopyFileRequest{
		From: boxEnv("priv"), FromPath: "/tmp/out", To: boxEnv("plain"), ToPath: "/tmp/in", RefuseSecrets: true, MaxBytes: int64(512 + 50 + len(copyToken) + 20),
	}))
	if r := refusalOf(err); r.GetReason() != envv1alpha1.CopyRefusalReason_COPY_REFUSAL_REASON_SECRET_FOUND {
		t.Errorf("a secret before the limit, in the write past it: %v", err)
	}
}

// TestACopyReplacesALinkAtItsDestination: a scanned copy to a path that
// is a link replaces the link, and writes nothing where it pointed.
func TestACopyReplacesALinkAtItsDestination(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.readyBox("a")
	dst := w.readyBox("b")
	ref := provider.Instance{Project: "web", Name: dst.GetStatus().GetInstance()}
	envs := w.as(workstream).envs
	ctx := t.Context()
	if err := writeFile(ctx, envs, boxEnv("a"), "/tmp/out/x", "built", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := exec(ctx, envs, &envv1alpha1.ExecRequest{Environment: boxEnv("b"), Command: argv("sh", "-c", `ln -sfn "$1" "$2"`, "sh", "/etc", "/tmp/target")}); err != nil {
		t.Fatal(err)
	}
	if w.fake.Links(ref)["/tmp/target"] != "/etc" {
		t.Fatalf("no link to begin with: %v", w.fake.Links(ref))
	}
	if _, err := envs.CopyFile(ctx, connect.NewRequest(&envv1alpha1.CopyFileRequest{
		From: boxEnv("a"), FromPath: "/tmp/out", To: boxEnv("b"), ToPath: "/tmp/target", MaxBytes: 1 << 20,
	})); err != nil {
		t.Fatal(err)
	}
	files := w.fake.Files(ref)
	if _, linked := w.fake.Links(ref)["/tmp/target"]; linked || files["/tmp/target/x"] != "built" || files["/etc/x"] != "" {
		t.Errorf("after the copy: links %v, /tmp/target/x %q, /etc/x %q", w.fake.Links(ref), files["/tmp/target/x"], files["/etc/x"])
	}
}

// TestACopyOutOfAMachine: a machine's blueprint secrets are what a copy
// out of it is scanned for, its tar run as its run_as user; one tower
// cannot open makes its copies unscannable.
func TestACopyOutOfAMachine(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.mustApply(registry.Secret, secret("web", "forge", map[string]string{"GH_TOKEN": copyToken}))
	m := machine("dev")
	m.Spec.Blueprint.RunAs = &envv1alpha1.RunAs{User: "dev"}
	m.Spec.Secrets = []*envv1alpha1.SecretRef{{Secret: "forge", Key: "GH_TOKEN", As: "GH_TOKEN"}}
	w.mustApply(registry.Machine, m)
	w.waitMachine("dev", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
	dst := w.readyBox("box", func(b *envv1alpha1.Box) { b.Spec.Workstream = "ws1" })
	src := provider.Instance{Project: "web", Name: w.machine("dev").GetStatus().GetInstance()}
	envs := w.as(workstream).envs
	ctx := t.Context()
	copyOut := func(content string) error {
		if err := w.fake.WriteFile(ctx, src, "/home/dev/out.txt", 0o644, 0, strings.NewReader(content)); err != nil {
			t.Fatal(err)
		}
		_, err := envs.CopyFile(ctx, connect.NewRequest(&envv1alpha1.CopyFileRequest{
			From: machineEnv("dev"), FromPath: "/home/dev/out.txt", To: boxEnv("box"), ToPath: "/tmp/in.txt", RefuseSecrets: true,
		}))
		return err
	}
	if r := refusalOf(copyOut("GH_TOKEN=" + copyToken)); r.GetReason() != envv1alpha1.CopyRefusalReason_COPY_REFUSAL_REASON_SECRET_FOUND || r.GetSecrets()[0] != "$GH_TOKEN" {
		t.Errorf("a machine's blueprint secret: %v", r)
	}
	if err := copyOut("clean"); err != nil || w.fake.Files(provider.Instance{Project: "web", Name: dst.GetStatus().GetInstance()})["/tmp/in.txt"] != "clean" {
		t.Errorf("a clean copy out of a machine: %v", err)
	}
	ran := false
	for _, e := range w.fake.Execs() {
		ran = ran || e.Instance == src && slices.Equal(e.Command[:min(5, len(e.Command))], []string{"runuser", "-u", "dev", "--", "tar"})
	}
	if !ran {
		t.Error("the machine's tar did not run as its run_as user")
	}
	if _, err := w.as(admin).secrets.DeleteSecret(ctx, connect.NewRequest(&secretsv1alpha1.DeleteSecretRequest{Project: "web", Name: "forge"})); err != nil {
		t.Fatal(err)
	}
	err := copyOut("clean")
	if r := refusalOf(err); r.GetReason() != envv1alpha1.CopyRefusalReason_COPY_REFUSAL_REASON_UNSCANNABLE || !slices.Equal(r.GetSecrets(), []string{"$GH_TOKEN"}) {
		t.Errorf("a machine whose secret cannot be opened: %v", err)
	}
}

// TestTheDestinationIdlesAfterACopy: a copy is a use of its destination
// that ends with the call, so the box is stopped for idleness after.
func TestTheDestinationIdlesAfterACopy(t *testing.T) {
	t.Parallel()
	w := newWorld(t, idleAfter(time.Hour))
	w.readyBox("a", func(b *envv1alpha1.Box) { b.Spec.Keep = true })
	w.readyBox("b")
	envs := w.as(workstream).envs
	ctx := t.Context()
	if err := writeFile(ctx, envs, boxEnv("a"), "/tmp/out", "built", 0); err != nil {
		t.Fatal(err)
	}
	for _, max := range []int64{0, 1 << 20} {
		if _, err := envs.CopyFile(ctx, connect.NewRequest(&envv1alpha1.CopyFileRequest{From: boxEnv("a"), FromPath: "/tmp/out", To: boxEnv("b"), ToPath: "/tmp/in", MaxBytes: max})); err != nil {
			t.Fatal(err)
		}
	}
	w.advance(61 * time.Minute)
	w.waitBox("b", envv1alpha1.BoxPhase_BOX_PHASE_STOPPED)
}

// TestACopyThatFailsLeavesNothing: a copy whose unpack fails, whose
// destination's write fails, or whose call ends midway, leaves nothing at
// its destination: its part, of a name of its own, and the file the guest
// writes the part through are removed.
func TestACopyThatFailsLeavesNothing(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.readyBox("a")
	dst := w.readyBox("b")
	ref := provider.Instance{Project: "web", Name: dst.GetStatus().GetInstance()}
	envs := w.as(workstream).envs
	ctx := t.Context()
	if err := writeFile(ctx, envs, boxEnv("a"), "/tmp/out", "built", 0); err != nil {
		t.Fatal(err)
	}
	copyFile := func(ctx context.Context) error {
		_, err := envs.CopyFile(ctx, connect.NewRequest(&envv1alpha1.CopyFileRequest{From: boxEnv("a"), FromPath: "/tmp/out", To: boxEnv("b"), ToPath: "/tmp/in", MaxBytes: 1 << 20}))
		return err
	}
	nothing := func(what string) {
		t.Helper()
		for name := range w.fake.Files(ref) {
			if strings.HasPrefix(name, "/tmp/in") {
				t.Errorf("%s left %s", what, name)
			}
		}
	}
	w.fake.FailCommands("sh")
	err := copyFile(ctx)
	w.fake.FailCommands()
	if wantCode(t, err, connect.CodeFailedPrecondition, "a failing unpack"); messageOfErr(err) != "copy failed: unpack exited 1" {
		t.Errorf("a failing unpack says %q", messageOfErr(err))
	}
	nothing("a failing unpack")
	w.fake.FailWriteFiles(errors.New("disk on fire"))
	if err := copyFile(ctx); err == nil {
		t.Error("a copy whose write failed went")
	}
	w.fake.FailWriteFiles(nil)
	nothing("a failing write")

	held, release := w.fake.HoldWriteFiles()
	defer release()
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- copyFile(cctx) }()
	<-held
	cancel()
	if err := <-done; err == nil {
		t.Error("a cancelled copy went")
	}
	part := regexp.MustCompile(`^/tmp/in\.part-[a-z2-7]{16}$`)
	guestTemp := regexp.MustCompile(`^\.in\.part-[a-z2-7]{16}\.tower-\*$`)
	testutil.Eventually(t, "the cancelled copy's part removed", func() (bool, string) {
		var removed, found []string
		for _, e := range w.fake.Execs() {
			switch {
			case e.Instance != ref || e.ExitCode != 0:
			case len(e.Command) == 5 && e.Command[0] == "rm" && part.MatchString(e.Command[3]) && e.Command[4] == e.Command[3]+".d":
				removed = append(removed, e.Command[3])
			case len(e.Command) == 7 && e.Command[0] == "find" && e.Command[1] == "/tmp" && guestTemp.MatchString(e.Command[5]):
				found = append(found, e.Command[5])
			}
		}
		// Three copies failed, each with a part of its own.
		return len(removed) == 3 && len(slices.Compact(slices.Sorted(slices.Values(removed)))) == 3 && len(found) == 3, fmt.Sprint(removed, found)
	})
	nothing("a cancelled copy")
}

// TestProbeRecreatedSecret: a Secret deleted and made again under its
// name holds none of the values the one before held, so a box given
// those is refused as unscannable, not copied out with them: the
// reviewer's probe. A Secret deleted and not made again is unscannable
// too.
func TestProbeRecreatedSecret(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	const old, renewed = "canary-old-Lz38mm", "canary-renewed-Vb46nn"
	w.mustApply(registry.Secret, secret("web", "forge", map[string]string{"GH_TOKEN": old}))
	w.mustApply(registry.Secret, secret("web", "deploy", map[string]string{"key": "canary-deploy-Kc19pp"}))
	w.advance(time.Minute)
	w.readyBox("priv", func(b *envv1alpha1.Box) {
		b.Spec.PrivilegedSecrets = []*envv1alpha1.SecretRef{{Secret: "forge", Key: "GH_TOKEN", As: "GH_TOKEN"}}
		b.Spec.SecretFiles = []*envv1alpha1.SecretFile{{Secret: "deploy", Key: "key", Path: "/etc/deploy/key"}}
	})
	w.readyBox("plain")
	w.advance(time.Minute)
	admin := w.as(admin).secrets
	ctx := t.Context()
	if _, err := admin.DeleteSecret(ctx, connect.NewRequest(&secretsv1alpha1.DeleteSecretRequest{Project: "web", Name: "forge"})); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "forge gone", func() bool {
		_, err := admin.GetSecret(ctx, connect.NewRequest(&secretsv1alpha1.GetSecretRequest{Project: "web", Name: "forge"}))
		return connect.CodeOf(err) == connect.CodeNotFound
	})
	w.advance(time.Minute)
	w.mustApply(registry.Secret, secret("web", "forge", map[string]string{"GH_TOKEN": renewed}))
	envs := w.as(workstream).envs
	if err := writeFile(ctx, envs, boxEnv("priv"), "/tmp/out", "token="+old, 0); err != nil {
		t.Fatal(err)
	}
	copyOut := func() *envv1alpha1.CopyRefusal {
		_, err := envs.CopyFile(ctx, connect.NewRequest(&envv1alpha1.CopyFileRequest{
			From: boxEnv("priv"), FromPath: "/tmp/out", To: boxEnv("plain"), ToPath: "/tmp/in", RefuseSecrets: true,
		}))
		if err == nil {
			t.Error("the box's old value was copied out")
		}
		return refusalOf(err)
	}
	if r := copyOut(); r.GetReason() != envv1alpha1.CopyRefusalReason_COPY_REFUSAL_REASON_UNSCANNABLE || !slices.Equal(r.GetSecrets(), []string{"$GH_TOKEN"}) {
		t.Errorf("a secret made again since the box was: %v", r)
	}
	if _, err := admin.DeleteSecret(ctx, connect.NewRequest(&secretsv1alpha1.DeleteSecretRequest{Project: "web", Name: "deploy"})); err != nil {
		t.Fatal(err)
	}
	if r := copyOut(); r.GetReason() != envv1alpha1.CopyRefusalReason_COPY_REFUSAL_REASON_UNSCANNABLE || !slices.Equal(r.GetSecrets(), []string{"$GH_TOKEN", "/etc/deploy/key"}) {
		t.Errorf("a secret deleted since: %v", r)
	}
	if files := w.fake.Files(provider.Instance{Project: "web", Name: w.box("plain").GetStatus().GetInstance()}); files["/tmp/in"] != "" {
		t.Errorf("the copy reached plain: %q", files["/tmp/in"])
	}
}
