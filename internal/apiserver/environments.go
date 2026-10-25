package apiserver

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	secretsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/secrets/v1alpha1"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/object"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/authz"
	"github.com/YukinaMochizuki/tower/internal/store"
)

// environmentHooks are the hooks of the kinds environments are made of,
// and ok is false for any other kind. Golden and Instance are tower's own:
// the role table lets no role write their spec. See
// docs/decisions/declarative-api.md and docs/decisions/placement.md.
func environmentHooks(r registry.Resource) (kindHooks, bool) {
	switch r.Kind {
	case registry.Target.Kind:
		return kindHooks{
			validate:     validateTarget,
			beforeDelete: refuseDeletingTargetInUse,
			targetOf:     func(obj proto.Message) string { return object.Meta(obj).GetName() },
		}, true
	case registry.Placement.Kind:
		return kindHooks{validate: validatePlacement}, true
	case registry.Repository.Kind:
		return kindHooks{owner: ownedByProject, validate: validateRepository}, true
	case registry.Box.Kind:
		return kindHooks{owner: ownedByProject, validate: validateBox}, true
	case registry.Machine.Kind:
		return kindHooks{owner: ownedByProject, validate: validateMachine}, true
	case registry.Golden.Kind:
		return kindHooks{
			owner:    ownedByProject,
			validate: validateGolden,
			targetOf: func(obj proto.Message) string { return obj.(*envv1alpha1.Golden).GetSpec().GetTarget() },
		}, true
	case registry.Instance.Kind:
		return kindHooks{
			owner:    ownedByProject,
			validate: validateInstance,
			targetOf: func(obj proto.Message) string { return obj.(*envv1alpha1.Instance).GetSpec().GetTarget() },
		}, true
	}
	return kindHooks{}, false
}

// providerOf names a target's provider, "" for none.
func providerOf(t *targetsv1alpha1.Target) string {
	switch t.GetSpec().GetProvider().(type) {
	case *targetsv1alpha1.TargetSpec_Hangar:
		return "hangar"
	case *targetsv1alpha1.TargetSpec_Hostd:
		return "hostd"
	}
	return ""
}

// validateTarget checks a target. Its provider never changes: a hangar
// target's endpoint and project are immutable, which refuses a change to
// or from hangar before this runs.
func validateTarget(_ context.Context, _ *Server, _ authz.Caller, _, obj proto.Message) error {
	t := obj.(*targetsv1alpha1.Target)
	switch providerOf(t) {
	case "":
		return invalid("spec.provider is required: hangar or hostd")
	case "hangar":
		h := t.GetSpec().GetHangar()
		if err := httpsURL(h.GetEndpoint()); err != nil {
			return invalid("spec.hangar.endpoint: %v", err)
		}
		if err := ValidName(h.GetProject()); err != nil {
			return invalid("spec.hangar.project: %v", err)
		}
		if err := ValidName(h.GetCredentialSecret()); err != nil {
			return invalid("spec.hangar.credential_secret: %v", err)
		}
	}
	set := t.GetSpec().GetSettings()
	for name, d := range map[string]*durationpb.Duration{
		"idle_stop_after": set.GetIdleStopAfter(), "warm_window": set.GetWarmWindow(), "warm_max_age": set.GetWarmMaxAge(),
	} {
		if d != nil && (!d.IsValid() || d.AsDuration() < 0) {
			return invalid("spec.settings.%s must be a duration of zero or more", name)
		}
	}
	if b := set.GetWarmBudget(); b != "" {
		if _, err := quantity.Bytes(b); err != nil {
			return invalid("spec.settings.warm_budget: %v", err)
		}
	}
	if set.GetWarmMaxCommitsBehind() < 0 {
		return invalid("spec.settings.warm_max_commits_behind must be zero or more")
	}
	return nil
}

// refuseDeletingTargetInUse refuses deleting a target a Placement or an
// Instance still names, saying which, to anyone but the system.
func refuseDeletingTargetInUse(ctx context.Context, s *Server, p authz.Caller, obj proto.Message) error {
	if p.Type == auth.System {
		return nil
	}
	name := object.Meta(obj).GetName()
	var users []string
	placements, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Placement.Kind})
	if err != nil {
		return err
	}
	for _, o := range placements {
		var pl targetsv1alpha1.Placement
		if err := proto.Unmarshal(o.Data, &pl); err != nil {
			return err
		}
		if pl.GetSpec().GetTarget() == name {
			users = append(users, "placement "+o.Name)
		}
	}
	instances, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.Instance.Kind})
	if err != nil {
		return err
	}
	for _, o := range instances {
		var in envv1alpha1.Instance
		if err := proto.Unmarshal(o.Data, &in); err != nil {
			return err
		}
		if in.GetSpec().GetTarget() == name {
			users = append(users, "instance "+o.Project+"/"+o.Name)
		}
	}
	if len(users) > 0 {
		return failedPrecondition("target %q is still in use by %s", name, strings.Join(users, ", "))
	}
	return nil
}

func validatePlacement(ctx context.Context, s *Server, _ authz.Caller, _, obj proto.Message) error {
	pl := obj.(*targetsv1alpha1.Placement)
	if err := s.requireLive(ctx, registry.Project, "", pl.GetMetadata().GetName()); err != nil {
		return invalid("a placement is named after its project: %s", messageOf(err))
	}
	if err := ValidName(pl.GetSpec().GetTarget()); err != nil {
		return invalid("spec.target: %v", err)
	}
	if err := s.requireLive(ctx, registry.Target, "", pl.GetSpec().GetTarget()); err != nil {
		return err
	}
	return nil
}

// httpsURL checks a URL is https, names a host, and carries no
// credentials, which belong in a Secret.
func httpsURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%q must be an https URL", raw)
	}
	if u.User != nil {
		return fmt.Errorf("%q must carry no credentials; name a Secret instead", u.Redacted())
	}
	return nil
}

func validateRepository(_ context.Context, _ *Server, _ authz.Caller, _, obj proto.Message) error {
	spec := obj.(*envv1alpha1.Repository).GetSpec()
	if err := httpsURL(spec.GetUrl()); err != nil {
		return invalid("spec.url: %v", err)
	}
	if c := spec.GetCredentialSecret(); c != "" {
		if err := ValidName(c); err != nil {
			return invalid("spec.credential_secret: %v", err)
		}
	}
	return nil
}

// validResources checks a shape's quantities parse; at names the field.
func validResources(at string, r *envv1alpha1.Resources) error {
	if c := r.GetCpu(); c != "" {
		if _, err := quantity.CPU(c); err != nil {
			return invalid("%s.cpu: %v", at, err)
		}
	}
	for field, q := range map[string]string{"memory": r.GetMemory(), "disk": r.GetDisk()} {
		if q == "" {
			continue
		}
		if _, err := quantity.Bytes(q); err != nil {
			return invalid("%s.%s: %v", at, field, err)
		}
	}
	return nil
}

// requireInProject checks, as an object is made, that the object of kind r
// it names lives in project.
func (s *Server) requireInProject(ctx context.Context, r registry.Resource, project, name, at string) error {
	if err := ValidName(name); err != nil {
		return invalid("%s: %v", at, err)
	}
	if err := s.requireLive(ctx, r, project, name); err != nil {
		return failedPrecondition("%s: %s in project %q", at, messageOf(err), project)
	}
	return nil
}

func validateBox(ctx context.Context, s *Server, _ authz.Caller, old, obj proto.Message) error {
	b := obj.(*envv1alpha1.Box)
	spec, project := b.GetSpec(), b.GetMetadata().GetProject()
	if err := ValidName(spec.GetWorkstream()); err != nil {
		return invalid("spec.workstream: %v", err)
	}
	if len(spec.GetCheckouts()) == 0 && spec.GetImage() == "" && spec.GetImageBuild() == nil {
		return invalid("a box with no checkout needs spec.image or spec.image_build")
	}
	if spec.GetImage() != "" && spec.GetImageBuild() != nil {
		return invalid("spec.image and spec.image_build are exclusive")
	}
	if b := spec.GetImageBuild(); b != nil {
		if err := validImageBuild(b); err != nil {
			return err
		}
	}
	var seen []string
	for i, c := range spec.GetCheckouts() {
		if slices.Contains(seen, c.GetRepository()) {
			return invalid("spec.checkouts: repository %q is checked out twice", c.GetRepository())
		}
		seen = append(seen, c.GetRepository())
		// What it names is checked as it is made: a repository deleted later
		// does not make the box's every apply fail.
		if old == nil {
			if err := s.requireInProject(ctx, registry.Repository, project, c.GetRepository(), fmt.Sprintf("spec.checkouts[%d].repository", i)); err != nil {
				return err
			}
		} else if err := ValidName(c.GetRepository()); err != nil {
			return invalid("spec.checkouts[%d].repository: %v", i, err)
		}
	}
	for _, a := range spec.GetAddons() {
		if err := ValidName(a); err != nil {
			return invalid("spec.addons: %v", err)
		}
	}
	var names []string
	for i, ref := range spec.GetPrivilegedSecrets() {
		at := fmt.Sprintf("spec.privileged_secrets[%d]", i)
		if !envName.MatchString(ref.GetAs()) {
			return invalid("%s.as must be an environment variable name", at)
		}
		if slices.Contains(names, ref.GetAs()) {
			return invalid("%s.as: %q is given twice", at, ref.GetAs())
		}
		names = append(names, ref.GetAs())
		if old == nil {
			if err := s.requireSecretKey(ctx, project, ref.GetSecret(), ref.GetKey(), at); err != nil {
				return err
			}
		}
	}
	var paths []string
	for i, f := range spec.GetSecretFiles() {
		at := fmt.Sprintf("spec.secret_files[%d]", i)
		if !path.IsAbs(f.GetPath()) || path.Clean(f.GetPath()) != f.GetPath() || f.GetPath() == "/" {
			return invalid("%s.path must be a clean absolute path of a file", at)
		}
		if slices.Contains(paths, f.GetPath()) {
			return invalid("%s.path: %q is given twice", at, f.GetPath())
		}
		paths = append(paths, f.GetPath())
		if f.GetMode() > 0o7777 {
			return invalid("%s.mode: %o is no file mode", at, f.GetMode())
		}
		if old == nil {
			if err := s.requireSecretKey(ctx, project, f.GetSecret(), f.GetKey(), at); err != nil {
				return err
			}
		}
	}
	return validResources("spec.resources", spec.GetResources())
}

// MaxImageBuild bounds an ImageBuild's Dockerfile and files, in all.
const MaxImageBuild = 16 << 20

// validImageBuild checks a box's own image build: a Dockerfile, and files
// at clean relative paths of its context, under MaxImageBuild in all.
func validImageBuild(b *envv1alpha1.ImageBuild) error {
	if strings.TrimSpace(b.GetDockerfile()) == "" {
		return invalid("spec.image_build.dockerfile is required")
	}
	size := len(b.GetDockerfile())
	for name, data := range b.GetFiles() {
		if name == "" || path.IsAbs(name) || path.Clean(name) != name || name == "." || name == ".." || strings.HasPrefix(name, "../") {
			return invalid("spec.image_build.files: %q is not a clean relative path", name)
		}
		if name == "Dockerfile" {
			return invalid("spec.image_build.files: the Dockerfile is spec.image_build.dockerfile")
		}
		size += len(name) + len(data)
	}
	if size > MaxImageBuild {
		return invalid("spec.image_build holds %d bytes, over the %d an image build may", size, MaxImageBuild)
	}
	return nil
}

// requireSecretKey checks, at at, that a Secret of the name is in the
// project and holds key. Only its keys are read, never its values.
func (s *Server) requireSecretKey(ctx context.Context, project, name, key, at string) error {
	if key == "" {
		return invalid("%s.key is required", at)
	}
	if err := s.requireInProject(ctx, registry.Secret, project, name, at+".secret"); err != nil {
		return err
	}
	sec, ok, err := getTyped[*secretsv1alpha1.Secret](ctx, s, registry.Secret, resource.Key{Project: project, Name: name})
	if err != nil {
		return err
	}
	if !ok || !slices.Contains(sec.GetStatus().GetKeys(), key) {
		return failedPrecondition("%s.key: secret %q holds no key %q", at, name, key)
	}
	return nil
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validateMachine(ctx context.Context, s *Server, _ authz.Caller, old, obj proto.Message) error {
	m := obj.(*envv1alpha1.Machine)
	spec, project := m.GetSpec(), m.GetMetadata().GetProject()
	if err := ValidName(spec.GetWorkstream()); err != nil {
		return invalid("spec.workstream: %v", err)
	}
	bp := spec.GetBlueprint()
	switch {
	case bp.GetName() == "":
		return invalid("spec.blueprint.name is required")
	case bp.GetContent() == "":
		return invalid("spec.blueprint.content is required")
	case bp.GetBaseImage() == "":
		return invalid("spec.blueprint.base_image is required")
	}
	if a := bp.GetNetAlias(); a != "" {
		if err := ValidName(a); err != nil {
			return invalid("spec.blueprint.net_alias: %v", err)
		}
	}
	for _, port := range bp.GetExposePorts() {
		if port < 1 || port > 65535 {
			return invalid("spec.blueprint.expose_ports: %d is not a port", port)
		}
	}
	if rc := bp.GetReadyCheck(); rc != nil {
		if strings.TrimSpace(rc.GetCommand()) == "" {
			return invalid("spec.blueprint.ready_check.command is required")
		}
		if t := rc.GetTimeout(); t != nil && (!t.IsValid() || t.AsDuration() <= 0) {
			return invalid("spec.blueprint.ready_check.timeout must be positive")
		}
	}
	var names []string
	for i, ref := range spec.GetSecrets() {
		at := fmt.Sprintf("spec.secrets[%d]", i)
		if ref.GetKey() == "" {
			return invalid("%s.key is required", at)
		}
		if !envName.MatchString(ref.GetAs()) {
			return invalid("%s.as must be an environment variable name", at)
		}
		if slices.Contains(names, ref.GetAs()) {
			return invalid("%s.as: %q is given twice", at, ref.GetAs())
		}
		names = append(names, ref.GetAs())
		if old == nil {
			if err := s.requireInProject(ctx, registry.Secret, project, ref.GetSecret(), at+".secret"); err != nil {
				return err
			}
		}
	}
	return validResources("spec.resources", spec.GetResources())
}

func validateGolden(_ context.Context, _ *Server, _ authz.Caller, _, obj proto.Message) error {
	spec := obj.(*envv1alpha1.Golden).GetSpec()
	if spec.GetTarget() == "" || spec.GetInputHash() == "" || spec.GetInputs() == nil {
		return invalid("a golden needs spec.target, spec.input_hash and its inputs")
	}
	return nil
}

func validateInstance(_ context.Context, _ *Server, _ authz.Caller, _, obj proto.Message) error {
	spec := obj.(*envv1alpha1.Instance).GetSpec()
	if spec.GetTarget() == "" || spec.GetKind() == envv1alpha1.InstanceKind_INSTANCE_KIND_UNSPECIFIED {
		return invalid("an instance needs spec.target and spec.kind")
	}
	return validResources("spec.resources", spec.GetResources())
}
