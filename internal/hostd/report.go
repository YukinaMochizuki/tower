package hostd

import (
	"context"
	"errors"
	"slices"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/tower/internal/client"
)

// TargetReporting says the provider reported within its interval.
const TargetReporting = "Reporting"

func (h *Host) getTarget(ctx context.Context) (*targetsv1alpha1.Target, error) {
	o, err := h.c.Get(ctx, registry.Target, client.Key{Name: h.cfg.Target})
	if err != nil {
		return nil, err
	}
	return o.(*targetsv1alpha1.Target), nil
}

// Report writes what hostd says of its host into the Target's status:
// capacity, facts, the disk its images hold (usage.images),
// last_report_time and Reporting, leaving what tower owns there as it is
// (internal/apiserver/targetstatus.go).
func (h *Host) Report(ctx context.Context) error {
	pool, err := h.d.Storage.Pool(ctx)
	if err != nil {
		return err
	}
	info, err := h.d.Host.Info(ctx)
	if err != nil {
		return err
	}
	facts := []*targetsv1alpha1.Fact{
		{Name: "hostd", Value: Version},
		{Name: "zfs pool", Value: pool.Name},
		{Name: "zfs health", Value: pool.Health},
	}
	versions := []struct {
		name string
		fn   func(context.Context) (string, error)
	}{
		{"storage driver", h.d.Storage.Version}, {"container driver", h.d.Containers.Version}, {"vm driver", h.d.VMs.Version},
		{"image driver", h.d.Images.Version}, {"git driver", h.d.Git.Version},
	}
	for _, v := range versions {
		s, err := v.fn(ctx)
		if err != nil {
			s = "unknown: " + err.Error()
		}
		facts = append(facts, &targetsv1alpha1.Fact{Name: v.name, Value: s})
	}
	imagesHeld, err := h.d.Images.DiskUsage(ctx)
	if err != nil {
		return err
	}
	capacity := &targetsv1alpha1.Capacity{
		DiskTotal: quantity.FormatDisk(pool.Size), DiskFree: quantity.FormatDisk(pool.Free),
		MemoryTotal: quantity.FormatMemory(info.MemoryTotal), MemoryFree: quantity.FormatMemory(info.MemoryFree), Cpus: int32(info.CPUs),
	}
	for range 10 {
		t, err := h.getTarget(ctx)
		if err != nil {
			return err
		}
		st := proto.Clone(t.GetStatus()).(*targetsv1alpha1.TargetStatus)
		if st == nil {
			st = &targetsv1alpha1.TargetStatus{}
		}
		st.Capacity, st.Facts, st.LastReportTime = capacity, facts, timestamppb.New(h.clock.Now())
		if st.Usage == nil {
			st.Usage = &targetsv1alpha1.Usage{}
		}
		st.Usage.Images = quantity.FormatDisk(imagesHeld)
		st.Conditions = conditions.Set(slices.Clone(st.GetConditions()), &metav1.Condition{
			Type: TargetReporting, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Reported",
			Message: "hostd " + Version + " reported",
		}, h.clock.Now())
		rv := t.GetMetadata().GetResourceVersion()
		_, err = h.c.UpdateStatus(ctx, registry.Target, client.Key{Name: h.cfg.Target}, st, &rv)
		if connect.CodeOf(err) == connect.CodeAborted {
			// tower wrote what it owns in between: read it again.
			continue
		}
		return err
	}
	return errors.New("the target's status kept changing under its report")
}
