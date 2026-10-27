package apiserver_test

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
	"google.golang.org/protobuf/encoding/prototext"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	secretsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/secrets/v1alpha1"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar/fake"
	"github.com/YukinaMochizuki/tower/internal/streams"
	"github.com/YukinaMochizuki/tower/internal/testutil"
	"github.com/YukinaMochizuki/tower/internal/ticket"
)

// TestTowerRunsAHangarTargetsProvider runs a hangar target's provider from
// its Target and the secret it names, over a fake hangar: the provider
// reports the target, makes its instances real, and tower serves direct
// streams to them, relayed to hangar's port forward. Without its
// credential the target has no provider, and says why.
func TestTowerRunsAHangarTargetsProvider(t *testing.T) {
	f := fake.New("", t.TempDir(), clockwork.NewRealClock())
	var mu sync.Mutex
	var tokens []string
	lastToken := func() string {
		mu.Lock()
		defer mu.Unlock()
		if len(tokens) == 0 {
			return ""
		}
		return tokens[len(tokens)-1]
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer := ticket.NewSigner(key)
	env := apiservertest.Start(t, func(c *apiserver.Config) {
		c.HangarAPI = func(endpoint, project, token string) hangar.API {
			mu.Lock()
			defer mu.Unlock()
			tokens = append(tokens, token)
			return f
		}
		c.Tickets = signer
		c.PublicURL = "https://tower.example"
	})
	env.Setup()
	admin := env.As(apiservertest.Admin)
	ctx := t.Context()
	if _, err := admin.Apply(ctx, registry.Project, &tenancyv1alpha1.Project{Metadata: &metav1.ObjectMeta{Name: "system"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Apply(ctx, registry.Target, &targetsv1alpha1.Target{
		Metadata: &metav1.ObjectMeta{Name: "h1"},
		Spec: &targetsv1alpha1.TargetSpec{Provider: &targetsv1alpha1.TargetSpec_Hangar{Hangar: &targetsv1alpha1.HangarTarget{
			Endpoint: "https://hangar.example", Project: fake.Project, CredentialSecret: "hangar-token",
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	target := func() *targetsv1alpha1.Target {
		o, err := admin.Get(ctx, registry.Target, client.Key{Name: "h1"})
		if err != nil {
			t.Fatal(err)
		}
		return o.(*targetsv1alpha1.Target)
	}
	testutil.Eventually(t, "the target saying it has no provider without its credential", func() (bool, string) {
		st := target().GetStatus()
		c := conditions.Find(st.GetConditions(), apiserver.TargetReachable)
		return c.GetReason() == "NoProvider", st.String()
	})

	if _, err := admin.Apply(ctx, registry.Secret, secret("system", "hangar-token", map[string]string{"token": "hangar-secret-1"})); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the provider reporting the target", func() (bool, string) {
		st := target().GetStatus()
		return conditions.IsTrue(st.GetConditions(), apiserver.TargetReachable) && conditions.IsTrue(st.GetConditions(), apiserver.TargetReporting), st.String()
	})
	if got := lastToken(); got != "hangar-secret-1" {
		t.Errorf("the provider was made with token %q", got)
	}

	if _, err := env.AsSystem().Apply(ctx, registry.Instance, &envv1alpha1.Instance{
		Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: "web"},
		Spec: &envv1alpha1.InstanceSpec{Target: "h1", Kind: envv1alpha1.InstanceKind_INSTANCE_KIND_CONTAINER,
			Image: "registry.tower.internal/box@sha256:00", Network: "ws1", Alias: "web"},
	}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the instance running", func() (bool, string) {
		o, err := env.AsSystem().Get(ctx, registry.Instance, client.Key{Project: apiservertest.Project, Name: "web"})
		if err != nil {
			t.Fatal(err)
		}
		st := o.(*envv1alpha1.Instance).GetStatus()
		return st.GetPhase() == envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING, st.String()
	})

	// The fake forwards a port to the host's: an echo server stands for
	// what the instance serves.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(c, c)
				_ = c.Close()
			}()
		}
	}()
	port := int32(ln.Addr().(*net.TCPAddr).Port)
	tk, err := signer.Issue(ticket.Claims{Target: "h1", Project: apiservertest.Project, Instance: "web", Port: port, Expires: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("%s%sh1/%s/web/%s", env.URL, apiserver.HangarStreamPath, apiservertest.Project, strconv.Itoa(int(port)))
	conn, err := streams.Dial(ctx, url, tk)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, "ping\n"); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(conn).ReadString('\n'); err != nil || line != "ping\n" {
		t.Errorf("the stream echoed %q, %v", line, err)
	}
	if _, err := streams.Dial(ctx, fmt.Sprintf("%s%sh1/%s/web/%d", env.URL, apiserver.HangarStreamPath, apiservertest.Project, port+1), tk); err == nil {
		t.Error("a ticket for one port opened another")
	}

	// A token rotated makes the provider again, with the new one.
	if _, err := admin.Apply(ctx, registry.Secret, secret("system", "hangar-token", map[string]string{"token": "hangar-secret-2"})); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the provider made again with the rotated token", func() (bool, string) {
		got := lastToken()
		return got == "hangar-secret-2", got
	})
}

// TestAHangarTargetsVMsGetAKeyTowerKeeps: tower makes a hangar target's
// key for its VMs' root once, as a Secret of the reserved project which
// no answer holds, and runs the target's VMs of its base images with it; a
// provider made again, as when its token rotates, keeps the key, and the
// Secret goes with the Target.
func TestAHangarTargetsVMsGetAKeyTowerKeeps(t *testing.T) {
	f := fake.New("", t.TempDir(), clockwork.NewRealClock())
	f.AddImage("ubuntu-24-04", "3Gi")
	env := apiservertest.Start(t, func(c *apiserver.Config) {
		c.HangarAPI = func(string, string, string) hangar.API { return f }
	})
	env.Setup()
	admin := env.As(apiservertest.Admin)
	ctx := t.Context()
	if _, err := admin.Apply(ctx, registry.Project, &tenancyv1alpha1.Project{Metadata: &metav1.ObjectMeta{Name: "system"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Apply(ctx, registry.Secret, secret("system", "hangar-token", map[string]string{"token": "hangar-secret-1"})); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Apply(ctx, registry.Target, &targetsv1alpha1.Target{
		Metadata: &metav1.ObjectMeta{Name: "h1"},
		Spec: &targetsv1alpha1.TargetSpec{Provider: &targetsv1alpha1.TargetSpec_Hangar{Hangar: &targetsv1alpha1.HangarTarget{
			Endpoint: "https://hangar.example", Project: fake.Project, CredentialSecret: "hangar-token",
			BaseImages: map[string]string{"ubuntu-24.04": "ubuntu-24-04"},
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	keySecret := func() (*secretsv1alpha1.Secret, error) {
		o, err := admin.Get(ctx, registry.Secret, client.Key{Project: "system", Name: "hangar-ssh-h1"})
		sec, _ := o.(*secretsv1alpha1.Secret)
		return sec, err
	}
	testutil.Eventually(t, "the target's ssh key made", func() (bool, string) {
		sec, err := keySecret()
		return err == nil && slices.Equal(sec.GetStatus().GetKeys(), []string{"SSH_PRIVATE_KEY"}), fmt.Sprint(sec, err)
	})
	if sec, _ := keySecret(); len(sec.GetSpec().GetData()) != 0 || strings.Contains(prototext.Format(sec), "PRIVATE KEY-----") {
		t.Errorf("an answer holds the key: %v", sec)
	}
	if _, err := env.AsSystem().Apply(ctx, registry.Instance, &envv1alpha1.Instance{
		Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: "vm"},
		Spec: &envv1alpha1.InstanceSpec{Target: "h1", Kind: envv1alpha1.InstanceKind_INSTANCE_KIND_VM, Image: "ubuntu-24.04", Network: "ws1", Alias: "vm",
			Resources: &envv1alpha1.Resources{Cpu: "1", Memory: "1Gi", Disk: "2Gi"}},
	}); err != nil {
		t.Fatal(err)
	}
	userData := func() string {
		for _, n := range f.VMNames() {
			if vm, err := f.VirtualMachines().Get(ctx, n); err == nil {
				return vm.GetSpec().GetUserData()
			}
		}
		return ""
	}
	testutil.Eventually(t, "the VM made with the target's key for root", func() (bool, string) {
		ud := userData()
		return strings.Contains(ud, " tower-hangar-h1\n"), ud
	})
	first := userData()
	if _, err := admin.Apply(ctx, registry.Secret, secret("system", "hangar-token", map[string]string{"token": "hangar-secret-2"})); err != nil {
		t.Fatal(err)
	}
	// The provider made again applies the VM as it is: the key it holds is
	// the one the VM was made with.
	if _, err := env.AsSystem().Apply(ctx, registry.Instance, &envv1alpha1.Instance{
		Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: "vm2"},
		Spec: &envv1alpha1.InstanceSpec{Target: "h1", Kind: envv1alpha1.InstanceKind_INSTANCE_KIND_VM, Image: "ubuntu-24.04", Network: "ws1", Alias: "vm2",
			Resources: &envv1alpha1.Resources{Cpu: "1", Memory: "1Gi", Disk: "2Gi"}},
	}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "both VMs made with one key", func() (bool, string) {
		var uds []string
		for _, n := range f.VMNames() {
			if vm, err := f.VirtualMachines().Get(ctx, n); err == nil {
				uds = append(uds, vm.GetSpec().GetUserData())
			}
		}
		return len(uds) == 2 && uds[0] == first && uds[1] == first, strings.Join(uds, "\n---\n")
	})

	for _, name := range []string{"vm", "vm2"} {
		if _, err := env.AsSystem().Delete(ctx, registry.Instance, client.Key{Project: apiservertest.Project, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.Delete(ctx, registry.Target, client.Key{Name: "h1"}); err != nil {
		t.Fatal(err)
	}
	// The teardown takes a pass every few seconds: the VMs, their disks,
	// then the rest.
	testutil.EventuallyWithin(t, time.Minute, "the key gone with the target", func() (bool, string) {
		_, err := keySecret()
		tg, terr := admin.Get(ctx, registry.Target, client.Key{Name: "h1"})
		return connect.CodeOf(err) == connect.CodeNotFound, fmt.Sprint(err, "; target: ", tg, terr)
	})
}
