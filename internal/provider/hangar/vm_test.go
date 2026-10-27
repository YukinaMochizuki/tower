package hangar_test

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar/hangartest"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

func vmSpec(network, alias string) *envv1alpha1.InstanceSpec {
	return &envv1alpha1.InstanceSpec{
		Target: hangartest.Target, Kind: envv1alpha1.InstanceKind_INSTANCE_KIND_VM, Image: "ubuntu-24.04",
		Network: network, Alias: alias, Resources: &envv1alpha1.Resources{Cpu: "2", Memory: "4Gi", Disk: "2Gi"},
	}
}

func authorizedLine(k ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
}

// TestWhatAVMIsInHangar: a VM instance is a VirtualMachine on its
// workstream's Network, named there by its alias, booting from a Disk of
// its own name made from the hangar Image its base image maps to, at least
// that image's size; cloud-init gives root the target's key and the
// hostname every tower VM has. Its VirtualMachine and Disk go with it.
func TestWhatAVMIsInHangar(t *testing.T) {
	w, f := newWorld(t, nil)
	h := w.Harness()
	ref, st := h.Start(t, "vm", vmSpec("ws1", "builder"))
	name := "t-" + w.Instance(t, "vm").GetMetadata().GetUid()
	vm, err := f.VirtualMachines().Get(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	spec := vm.GetSpec()
	nets, err := f.Networks().List(t.Context(), hangar.LabelNetwork+"=ws1")
	if err != nil || len(nets) != 1 {
		t.Fatalf("networks %v, %v", nets, err)
	}
	if spec.GetRootDisk().GetDisk() != name || spec.GetResources().GetCpu() != "2" || spec.GetResources().GetMemory() != "4Gi" {
		t.Errorf("the VM's spec: %v", spec)
	}
	if ifs := spec.GetInterfaces(); len(ifs) != 1 || ifs[0].GetNetwork() != nets[0].GetMetadata().GetName() || len(ifs[0].GetAliases()) != 1 || ifs[0].GetAliases()[0] != "builder" {
		t.Errorf("the VM's interfaces: %v, want one on %s named builder", ifs, nets[0].GetMetadata().GetName())
	}
	key, err := ssh.ParsePrivateKey(w.Config.SSHKey)
	if err != nil {
		t.Fatal(err)
	}
	ud := spec.GetUserData()
	if !strings.Contains(ud, "  - "+authorizedLine(key.PublicKey())) || !strings.Contains(ud, "disable_root: false\n") || !strings.Contains(ud, "hostname: machine\n") {
		t.Errorf("the VM's user_data does not give root the target's key and the hostname:\n%s", ud)
	}
	disk, err := f.Disks().Get(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	if src := disk.GetSpec().GetSource().GetImage(); src.GetName() != "ubuntu-24-04" || src.GetProject() != "" || disk.GetSpec().GetSize() != "3Gi" {
		t.Errorf("the VM's disk: %v, want ubuntu-24-04's, as big as it is (3Gi)", disk.GetSpec())
	}
	if st.GetAddress() != "127.0.0.1" {
		t.Errorf("the VM's address: %q", st.GetAddress())
	}
	var hostname bytes.Buffer
	if err := w.Target.ReadFile(t.Context(), ref, "/etc/hostname", &hostname); err != nil || hostname.String() != "machine\n" {
		t.Errorf("the VM's hostname: %q, %v", hostname.String(), err)
	}
	// Deleted, it goes, its disk once it has.
	if _, err := w.Env.AsSystem().Delete(t.Context(), registry.Instance, client.Key{Project: ref.Project, Name: ref.Name}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the VM and its disk gone", func() (bool, string) {
		return !strSet(f.VMNames()).has(name) && !strSet(f.DiskNames()).has(name), strings.Join(append(f.VMNames(), f.DiskNames()...), ",")
	})
}

// TestAContainersAliasNamesItOnItsNetwork: a container's alias is its
// interface's, as a VM's is.
func TestAContainersAliasNamesItOnItsNetwork(t *testing.T) {
	w, f := newWorld(t, nil)
	w.Harness().Start(t, "box", container("ws1"))
	ct, err := f.Containers().Get(t.Context(), "t-"+w.Instance(t, "box").GetMetadata().GetUid())
	if err != nil {
		t.Fatal(err)
	}
	if ifs := ct.GetSpec().GetInterfaces(); len(ifs) != 1 || len(ifs[0].GetAliases()) != 1 || ifs[0].GetAliases()[0] != "box" {
		t.Errorf("the container's interfaces: %v, want one named box", ifs)
	}
}

// TestAVMsHostKeyIsPinnedAsItFirstAnswers: the first connection to a VM
// pins the host key it answered with on its Instance; a later one that
// answers with another is refused, naming the instance and both keys, and
// the pin stays.
func TestAVMsHostKeyIsPinnedAsItFirstAnswers(t *testing.T) {
	w, f := newWorld(t, nil)
	ref, _ := w.Harness().Start(t, "vm", vmSpec("ws1", "vm"))
	name := "t-" + w.Instance(t, "vm").GetMetadata().GetUid()
	if pin := w.Instance(t, "vm").GetStatus().GetSshHostKey(); pin != "" {
		t.Fatalf("a VM never connected to pins %q", pin)
	}
	if _, err := w.Target.Exec(t.Context(), provider.ExecRequest{Instance: ref, Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	first := f.VMHostKey(name)
	if pin := w.Instance(t, "vm").GetStatus().GetSshHostKey(); pin != authorizedLine(first) {
		t.Fatalf("the instance pins %q, want the key the VM answered with, %q", pin, authorizedLine(first))
	}
	if err := f.ChangeHostKey(name); err != nil {
		t.Fatal(err)
	}
	_, err := w.Target.Exec(t.Context(), provider.ExecRequest{Instance: ref, Command: []string{"true"}})
	if !errors.Is(err, provider.ErrFailed) {
		t.Fatalf("a VM answering with another key: %v, want ErrFailed", err)
	}
	for _, want := range []string{ref.Project + "/" + ref.Name, ssh.FingerprintSHA256(first), ssh.FingerprintSHA256(f.VMHostKey(name))} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
	if pin := w.Instance(t, "vm").GetStatus().GetSshHostKey(); pin != authorizedLine(first) {
		t.Errorf("a refused key changed the pin to %q", pin)
	}
}

// TestAPinOutlivesTower: a provider made anew, as after tower restarts,
// speaks to a VM only with the key its Instance pins.
func TestAPinOutlivesTower(t *testing.T) {
	w, f := newWorld(t, nil)
	ref, _ := w.Harness().Start(t, "vm", vmSpec("ws1", "vm"))
	name := "t-" + w.Instance(t, "vm").GetMetadata().GetUid()
	if _, err := w.Target.Exec(t.Context(), provider.ExecRequest{Instance: ref, Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	again := hangar.New(w.Config)
	if _, err := again.Exec(t.Context(), provider.ExecRequest{Instance: ref, Command: []string{"true"}}); err != nil {
		t.Fatalf("the pinned VM after a restart: %v", err)
	}
	if err := f.ChangeHostKey(name); err != nil {
		t.Fatal(err)
	}
	restarted := hangar.New(w.Config)
	if _, err := restarted.Exec(t.Context(), provider.ExecRequest{Instance: ref, Command: []string{"true"}}); !errors.Is(err, provider.ErrFailed) || !strings.Contains(fmtErr(err), "first answered with") {
		t.Errorf("a VM answering with another key after a restart: %v, want it refused", err)
	}
}

func fmtErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// TestAVMClonedFromAGoldenPinsItsOwnKey: a clone of a VM's golden is a new
// instance, whose host key is its own, pinned on its own Instance, with the
// hostname every tower VM has.
func TestAVMClonedFromAGoldenPinsItsOwnKey(t *testing.T) {
	w, f := newWorld(t, nil)
	h := w.Harness()
	src, _ := h.Start(t, "builder", vmSpec("ws1", "builder"))
	if _, err := w.Target.Exec(t.Context(), provider.ExecRequest{Instance: src, Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	h.Stop(t, src)
	art, err := w.Target.Capture(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	spec := vmSpec("ws1", "clone")
	spec.DiskGolden = h.Golden(t, art)
	clone, _ := h.Start(t, "clone", spec)
	var hostname bytes.Buffer
	if err := w.Target.ReadFile(t.Context(), clone, "/etc/hostname", &hostname); err != nil || hostname.String() != "machine\n" {
		t.Errorf("the clone's hostname: %q, %v", hostname.String(), err)
	}
	builderPin, clonePin := w.Instance(t, "builder").GetStatus().GetSshHostKey(), w.Instance(t, "clone").GetStatus().GetSshHostKey()
	cloneKey := f.VMHostKey("t-" + w.Instance(t, "clone").GetMetadata().GetUid())
	if clonePin == "" || clonePin == builderPin || clonePin != authorizedLine(cloneKey) {
		t.Errorf("the clone pins %q, the builder %q; want the clone's own key %q", clonePin, builderPin, authorizedLine(cloneKey))
	}
}

// TestAVMOfABaseImageHangarLacksFails: a base image the target maps to no
// hangar Image, or to one hangar does not have, fails its instance, saying
// which.
func TestAVMOfABaseImageHangarLacksFails(t *testing.T) {
	w, _ := newWorld(t, func(c *hangar.Config) {
		c.BaseImages = map[string]string{"ubuntu-24.04": "ubuntu-24-04", "debian-13": "debian-13"}
	})
	for name, image := range map[string]string{"unmapped": "rocky-9", "missing": "debian-13"} {
		spec := vmSpec("ws1", name)
		spec.Image = image
		if _, err := w.Env.AsSystem().Apply(t.Context(), registry.Instance, &envv1alpha1.Instance{Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: name}, Spec: spec}); err != nil {
			t.Fatal(err)
		}
	}
	for name, reason := range map[string]string{"unmapped": "UnknownBaseImage", "missing": "BaseImageNotInHangar"} {
		in := w.WaitPhase(t, name, envv1alpha1.InstancePhase_INSTANCE_PHASE_FAILED)
		if c := conditions.Find(in.GetStatus().GetConditions(), hangar.InstanceReady); c.GetReason() != reason {
			t.Errorf("instance %s failed as %v, want %s", name, c, reason)
		}
	}
}

// TestTheTargetsSSHKeyIsNowhere: the target's private key is in no hangar
// object, the VM's user_data among them, in no Instance, and in no log
// line, as a VM runs, is pinned and refused.
func TestTheTargetsSSHKeyIsNowhere(t *testing.T) {
	var logs syncBuffer
	w, f := newWorld(t, func(c *hangar.Config) {
		c.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	ref, _ := w.Harness().Start(t, "vm", vmSpec("ws1", "vm"))
	if _, err := w.Target.Exec(t.Context(), provider.ExecRequest{Instance: ref, Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	name := "t-" + w.Instance(t, "vm").GetMetadata().GetUid()
	if err := f.ChangeHostKey(name); err != nil {
		t.Fatal(err)
	}
	_, refusal := w.Target.Exec(t.Context(), provider.ExecRequest{Instance: ref, Command: []string{"true"}})
	var all strings.Builder
	all.WriteString(fmtErr(refusal))
	all.WriteString(logs.String())
	all.WriteString(prototext.Format(w.Instance(t, "vm")))
	dump := func(msgs ...proto.Message) {
		for _, m := range msgs {
			all.WriteString(prototext.Format(m))
		}
	}
	ctx := t.Context()
	vms, _ := f.VirtualMachines().List(ctx, "")
	disks, _ := f.Disks().List(ctx, "")
	cts, _ := f.Containers().List(ctx, "")
	secs, _ := f.Secrets().List(ctx, "")
	nets, _ := f.Networks().List(ctx, "")
	for _, v := range vms {
		dump(v)
	}
	for _, d := range disks {
		dump(d)
	}
	for _, c := range cts {
		dump(c)
	}
	for _, s := range secs {
		dump(f.Secret(s.GetMetadata().GetName()))
	}
	for _, n := range nets {
		dump(n)
	}
	body := pemBody(w.Config.SSHKey)
	if len(body) < 64 {
		t.Fatalf("the key's body %q is too short to look for", body)
	}
	for _, line := range strings.Split(body, "\n") {
		if len(line) >= 16 && strings.Contains(all.String(), line) {
			t.Errorf("the target's private key is out: a line of it, %q, is in what tower wrote", line)
		}
	}
	if strings.Contains(all.String(), "PRIVATE KEY") {
		t.Error("a private key is in what tower wrote")
	}
	if len(vms) == 0 {
		t.Error("no VM was looked at")
	}
}

// pemBody is a PEM's base64 lines.
func pemBody(b []byte) string {
	var lines []string
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" && !strings.HasPrefix(l, "-----") {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
