package hangar

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	hangarcompute "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	hangarmeta "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	hangarstorage "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider"
)

// A golden on a hangar target is a hangar CustomImage, captured from a
// stopped instance's Disk: a copy of it the node makes on its own, which
// outlives the disk (hangar: docs/decisions/custom-images.md). A disk
// cloned from a golden is a Disk made from that image.

// goldenName is the CustomImage captured from an instance's disk.
func goldenName(uid string) string { return "g-" + uid }

// captureEvery is how often a capture waiting on hangar reads it again.
const captureEvery = 2 * time.Second

func (t *Target) Capture(ctx context.Context, in provider.Instance) (provider.Artifact, error) {
	p, err := t.find(ctx, in)
	if err != nil {
		return provider.Artifact{}, err
	}
	uid := p.in.GetMetadata().GetUid()
	name := goldenName(uid)
	img, err := t.api.CustomImages().Get(ctx, name)
	if errors.Is(err, ErrNotFound) {
		ct, err := t.api.Containers().Get(ctx, p.container)
		if err != nil {
			return provider.Artifact{}, fmt.Errorf("instance %s/%s: %w", in.Project, in.Name, providerError(err))
		}
		m, err := t.api.Machine(ctx, machineOf(p.container))
		if err == nil && ownedBy(m, ct) && !machineStopped(m) {
			return provider.Artifact{}, fmt.Errorf("instance %s/%s is running; capture takes a stopped one: %w", in.Project, in.Name, provider.ErrFailed)
		}
		labels := t.labels(p.in)
		img, err = t.api.CustomImages().Apply(ctx, &hangarstorage.CustomImage{
			Metadata: &hangarmeta.ObjectMeta{Name: name, Labels: labels},
			Spec: &hangarstorage.CustomImageSpec{Source: &hangarstorage.CustomImageSource{
				// The instance's disk is named as its container is.
				Source: &hangarstorage.CustomImageSource_Disk{Disk: &hangarstorage.CustomImageDiskSource{Name: p.container}},
			}},
		})
		if err != nil {
			return provider.Artifact{}, fmt.Errorf("capturing instance %s/%s: %w", in.Project, in.Name, providerError(err))
		}
	} else if err != nil {
		return provider.Artifact{}, providerError(err)
	}
	// A capture is a copy hangar's node makes; it is waited for.
	for {
		if img.GetMetadata().GetDeleteTime() != nil {
			return provider.Artifact{}, fmt.Errorf("instance %s/%s's image %s is being deleted; it is captured again once it is gone: %w", in.Project, in.Name, name, provider.ErrUnavailable)
		}
		ready := conditionOf(img.GetStatus().GetConditions(), "Ready")
		switch {
		case ready.GetStatus() == hangarmeta.ConditionStatus_CONDITION_STATUS_TRUE:
			used, _ := quantity.Bytes(img.GetStatus().GetUsedSize())
			return provider.Artifact{Name: name, Size: used}, nil
		case ready.GetReason() == "QuotaExceeded":
			return provider.Artifact{}, fmt.Errorf("capturing instance %s/%s: %s: %w", in.Project, in.Name, ready.GetMessage(), provider.ErrNoSpace)
		case ready.GetReason() == "CaptureFailed":
			return provider.Artifact{}, fmt.Errorf("capturing instance %s/%s: %s: %w", in.Project, in.Name, ready.GetMessage(), provider.ErrFailed)
		}
		select {
		case <-ctx.Done():
			return provider.Artifact{}, ctx.Err()
		case <-t.clock.After(captureEvery):
		}
		if img, err = t.api.CustomImages().Get(ctx, name); err != nil {
			return provider.Artifact{}, fmt.Errorf("capturing instance %s/%s: %w", in.Project, in.Name, providerError(err))
		}
	}
}

// machineStopped reports whether hangar's node has stopped a machine, or
// it failed: what a capture takes.
func machineStopped(m *hangarcompute.Machine) bool {
	switch m.GetStatus().GetPhase() {
	case hangarcompute.MachinePhase_MACHINE_PHASE_STOPPED, hangarcompute.MachinePhase_MACHINE_PHASE_FAILED:
		return true
	}
	return false
}

// DeleteArtifact deletes a golden this target captured; one already gone,
// or being deleted, is no error, and hangar keeps its base until the disks
// made from it go.
func (t *Target) DeleteArtifact(ctx context.Context, artifact string) error {
	img, err := t.api.CustomImages().Get(ctx, artifact)
	// A name hangar refuses names no image there is.
	if errors.Is(err, ErrNotFound) || connect.CodeOf(err) == connect.CodeInvalidArgument {
		return nil
	}
	if err != nil {
		return providerError(err)
	}
	if !t.mine(img.GetMetadata().GetLabels()) {
		return fmt.Errorf("artifact %s is not this target's to delete: %w", artifact, provider.ErrFailed)
	}
	return providerError(t.api.CustomImages().Delete(ctx, artifact))
}

// goldenSource is the source of a disk cloned from the golden g, and the
// disk's size: at least the image's. While the image is not ready the
// instance waits, and once it is gone or failed it fails.
func (t *Target) goldenSource(ctx context.Context, in *envv1alpha1.Instance, g, size string) (*hangarstorage.DiskSource, string, *observed, error) {
	o, err := t.c.Get(ctx, registry.Golden, client.Key{Project: in.GetMetadata().GetProject(), Name: g})
	if err != nil && connect.CodeOf(err) != connect.CodeNotFound {
		return nil, "", nil, err
	}
	gd, _ := o.(*envv1alpha1.Golden)
	if gd.GetStatus().GetPhase() != envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY || gd.GetStatus().GetArtifact() == "" {
		w := pending("WaitingForGolden", "its golden "+g+" is not ready")
		return nil, "", &w, nil
	}
	art := gd.GetStatus().GetArtifact()
	img, err := t.api.CustomImages().Get(ctx, art)
	if errors.Is(err, ErrNotFound) || (err == nil && img.GetMetadata().GetDeleteTime() != nil) {
		f := failed("GoldenNotOnTarget", fmt.Errorf("its golden %s's image %s is not in hangar", g, art))
		return nil, "", &f, nil
	}
	if err != nil {
		return nil, "", nil, err
	}
	if min := img.GetStatus().GetDiskSize(); min != "" {
		want, werr := quantity.Bytes(size)
		have, herr := quantity.Bytes(min)
		if werr == nil && herr == nil && have > want {
			size = min
		}
	}
	return &hangarstorage.DiskSource{Source: &hangarstorage.DiskSource_Image{Image: &hangarstorage.ImageSource{Name: art, Project: t.api.ProjectName()}}}, size, nil, nil
}
