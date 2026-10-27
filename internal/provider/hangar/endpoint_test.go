package hangar_test

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hangartenancy "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"

	"github.com/YukinaMochizuki/tower/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/tower/internal/guest"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar/fake"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar/hangartest"
)

// fetched fetches the world's remote into a mirror of the world's
// project, and returns its name.
func fetched(t *testing.T, w *hangartest.World, repo string) string {
	t.Helper()
	h := w.Harness()
	mirror := apiservertest.Project + "/" + repo
	if _, err := w.Target.Fetch(t.Context(), provider.FetchRequest{Mirror: mirror, URL: h.Remote, Branch: h.Branch}); err != nil {
		t.Fatal(err)
	}
	return mirror
}

// TestATransferTheEndpointCannotCarryIsRelayed: a checkout whose network's
// endpoint is not Connected, whose probe from the box fails while hangar
// says it is Connected (hangar#226), or whose network hangar gives no
// endpoint, as one out of its quota, is relayed through tower, and the
// target says why.
func TestATransferTheEndpointCannotCarryIsRelayed(t *testing.T) {
	cases := []struct {
		name, why string
		setup     func(*fake.Fake)
	}{
		{"not connected", "not connected: NotConnected: across nodes", func(f *fake.Fake) { f.SetEndpointsDown("across nodes") }},
		{"a probe that fails", "the probe from the box", func(f *fake.Fake) { f.DropEndpointTraffic(true) }},
		{"out of quota", "quota of network endpoints", func(f *fake.Fake) {
			none := int32(0)
			f.SetLimits(&hangartenancy.Limits{NetworkEndpoints: &none})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, _ := newWorld(t, nil, c.setup)
			mirror := fetched(t, w, "api")
			in, _ := w.Harness().Start(t, "box", container("ws1"))
			if _, err := w.Target.Checkout(t.Context(), in, mirror, "", "/workspace"); err != nil {
				t.Fatal(err)
			}
			var head bytes.Buffer
			if err := w.Target.ReadFile(t.Context(), in, "/workspace/.git/HEAD", &head); err != nil {
				t.Errorf("the relayed checkout: %v", err)
			}
			m := w.Target.MirrorTransfers()
			if m.Direct != 0 || m.Relayed != 1 || m.RelayedBytes == 0 || !strings.Contains(m.LastRelay, c.why) {
				t.Errorf("the transfers: %+v, want one relayed as %q", m, c.why)
			}
			if err := w.Target.Report(t.Context()); err != nil {
				t.Fatal(err)
			}
			if v := fact(t, w, "mirror transfers"); !strings.Contains(v, "1 relayed") || !strings.Contains(v, c.why) {
				t.Errorf("the target's mirror transfers: %q", v)
			}
		})
	}
}

// TestNoCredentialNorTokenReachesTheBox: a checkout, an origin and a push
// over the endpoint leave neither their credentials nor the repository's
// token anywhere in the box's filesystem, nor in tower's log; each
// credential reached the box only in its tower-guest call's header.
func TestNoCredentialNorTokenReachesTheBox(t *testing.T) {
	var log syncBuffer
	w, f := newWorld(t, func(c *hangar.Config) {
		c.Log = slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	ctx := t.Context()
	mirror := fetched(t, w, "api")
	in, _ := w.Harness().Start(t, "box", container("ws1"))
	if _, err := w.Target.Checkout(ctx, in, mirror, "", "/workspace"); err != nil {
		t.Fatal(err)
	}
	if err := w.Target.UpdateOrigin(ctx, in, mirror, "/workspace"); err != nil {
		t.Fatal(err)
	}
	const token = "repository-token-never-in-a-box"
	h := w.Harness()
	if err := w.Target.Push(ctx, provider.PushRequest{Instance: in, Dir: "/workspace", Mirror: mirror, URL: h.Remote, Token: token, Branch: "elsewhere"}); err != nil {
		t.Fatal(err)
	}
	if m := w.Target.MirrorTransfers(); m.Direct != 3 || m.Relayed != 0 {
		t.Fatalf("the transfers: %+v, want three over the endpoint", m)
	}
	secrets := append(f.Credentials(), token)
	if len(secrets) != 4 {
		t.Fatalf("the shims carried %d credentials, want 3", len(secrets)-1)
	}
	root := f.ContainerRoot("t-" + w.Instance(t, "box").GetMetadata().GetUid())
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, s := range secrets {
			if bytes.Contains(b, []byte(s)) {
				t.Errorf("%s holds a secret", strings.TrimPrefix(p, root))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range secrets {
		if strings.Contains(log.String(), s) {
			t.Error("tower's log holds a secret")
		}
	}
}

// TestABoxTakesOnlyItsProjectsMirrors: a box's checkout, origin and push
// of another project's mirror over the endpoint are refused by the
// server, their own credentials notwithstanding, and not relayed instead.
func TestABoxTakesOnlyItsProjectsMirrors(t *testing.T) {
	w, _ := newWorld(t, nil)
	ctx := t.Context()
	h := w.Harness()
	if _, err := w.Target.Fetch(ctx, provider.FetchRequest{Mirror: "other/api", URL: h.Remote, Branch: h.Branch}); err != nil {
		t.Fatal(err)
	}
	in, _ := h.Start(t, "box", container("ws1"))
	_, err := w.Target.Checkout(ctx, in, "other/api", "", "/workspace")
	if !errors.Is(err, provider.ErrFailed) || !strings.Contains(err.Error(), "not project web's") {
		t.Errorf("a checkout of another project's mirror: %v, want it refused", err)
	}
	// A checkout of its own to update and push from.
	if _, err := w.Target.Checkout(ctx, in, fetched(t, w, "api"), "", "/workspace"); err != nil {
		t.Fatal(err)
	}
	err = w.Target.UpdateOrigin(ctx, in, "other/api", "/workspace")
	if !errors.Is(err, provider.ErrFailed) || !strings.Contains(err.Error(), "not project web's") {
		t.Errorf("an origin of another project's mirror: %v, want it refused", err)
	}
	err = w.Target.Push(ctx, provider.PushRequest{Instance: in, Dir: "/workspace", Mirror: "other/api", URL: h.Remote, Branch: "elsewhere"})
	if !errors.Is(err, provider.ErrFailed) || !strings.Contains(err.Error(), "not project web's") {
		t.Errorf("a push through another project's mirror: %v, want it refused", err)
	}
	if m := w.Target.MirrorTransfers(); m.Relayed != 0 {
		t.Errorf("the transfers: %+v, want none relayed", m)
	}
}

// TestACredentialIsRefusedFromAnotherNetwork: a credential for one
// network's box, presented by a box of another network over its own
// endpoint, is refused by the server; the probe already refuses a box
// that names a network not its own.
func TestACredentialIsRefusedFromAnotherNetwork(t *testing.T) {
	w, _ := newWorld(t, nil)
	ctx := t.Context()
	mirror := fetched(t, w, "api")
	h := w.Harness()
	a, _ := h.Start(t, "a", container("ws1"))
	spec := container("ws2")
	spec.Alias = "b"
	b, _ := h.Start(t, "b", spec)
	if ans, err := w.Target.PresentElsewhere(ctx, a, a, a, mirror); err != nil || ans.Status != http.StatusOK {
		t.Fatalf("the owner's own box: %+v, %v", ans, err)
	}
	if ans, err := w.Target.PresentElsewhere(ctx, a, b, b, mirror); err != nil || ans.Status != http.StatusForbidden || !strings.Contains(ans.Message, "not from network") {
		t.Errorf("another network's box: %+v, %v; want 403", ans, err)
	}
	if _, err := w.Target.PresentElsewhere(ctx, a, b, a, mirror); !errors.Is(err, guest.ErrUnreachable) {
		t.Errorf("another network's box probing as the owner's: %v, want the probe refused", err)
	}
}

// TestAProbeThatFailedIsNotWaitedOnAgain: an endpoint hangar says is
// Connected that takes connections and answers nothing costs the first
// transfer its probe's timeout, which is relayed; the network's next
// transfers are relayed at once, saying why, until the probe is tried
// again.
func TestAProbeThatFailedIsNotWaitedOnAgain(t *testing.T) {
	w, _ := newWorld(t, nil, func(f *fake.Fake) { f.BlackholeEndpoints(true) })
	ctx := t.Context()
	mirror := fetched(t, w, "api")
	in, _ := w.Harness().Start(t, "box", container("ws1"))
	start := time.Now()
	if _, err := w.Target.Checkout(ctx, in, mirror, "", "/workspace"); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < 10*time.Second {
		t.Fatalf("the first checkout took %s: the probe did not wait on the endpoint", took)
	}
	start = time.Now()
	if err := w.Target.UpdateOrigin(ctx, in, mirror, "/workspace"); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the next transfer took %s, waiting on the probe again", took)
	}
	if m := w.Target.MirrorTransfers(); m.Direct != 0 || m.Relayed != 2 || !strings.Contains(m.LastRelay, "the probe from the box") || !strings.Contains(m.LastRelay, "tried again after 5m0s") {
		t.Errorf("the transfers: %+v, want both relayed, the second for the probe that failed", m)
	}
}

// TestAServerThatStopsAnsweringIsRelayedTo: once the system container's
// server stops answering, its check says so, and the transfers are
// relayed, saying why.
func TestAServerThatStopsAnsweringIsRelayedTo(t *testing.T) {
	w, f := newWorld(t, nil)
	ctx := t.Context()
	mirror := fetched(t, w, "api")
	in, _ := w.Harness().Start(t, "box", container("ws1"))
	f.SetServerDown(true)
	if err := w.Target.EnsureSystem(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Target.Checkout(ctx, in, mirror, "", "/workspace"); err != nil {
		t.Fatal(err)
	}
	const why = "tower-system serve does not answer in the system container"
	if m := w.Target.MirrorTransfers(); m.Direct != 0 || m.Relayed != 1 || m.LastRelay != why {
		t.Errorf("the transfers: %+v, want one relayed as %q", m, why)
	}
	if err := w.Target.Report(ctx); err != nil {
		t.Fatal(err)
	}
	if v := fact(t, w, "mirror transport"); v != "relay: "+why {
		t.Errorf("the target's mirror transport: %q", v)
	}
}

// TestACredentialNotPresentedIsTakenBack: a transfer granted a credential
// and relayed after all, its probe failing, takes the credential back: the
// box presenting it later is refused.
func TestACredentialNotPresentedIsTakenBack(t *testing.T) {
	w, f := newWorld(t, nil, func(f *fake.Fake) { f.DropEndpointTraffic(true) })
	ctx := t.Context()
	mirror := fetched(t, w, "api")
	in, _ := w.Harness().Start(t, "box", container("ws1"))
	if _, err := w.Target.Checkout(ctx, in, mirror, "", "/workspace"); err != nil {
		t.Fatal(err)
	}
	creds := f.Credentials()
	if m := w.Target.MirrorTransfers(); m.Relayed != 1 || len(creds) != 1 {
		t.Fatalf("the transfers: %+v, credentials %d; want one relayed, granted one", m, len(creds))
	}
	f.DropEndpointTraffic(false)
	if ans, err := w.Target.Present(ctx, in, creds[0]); err != nil || ans.Status != http.StatusForbidden {
		t.Errorf("the credential presented after its transfer was relayed: %+v, %v; want 403", ans, err)
	}
}
