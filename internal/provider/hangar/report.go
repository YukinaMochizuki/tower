package hangar

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	hangartenancy "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"

	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/tower/internal/client"
)

// Conditions the provider owns on its Target's status
// (internal/apiserver/targetstatus.go).
const (
	// TargetReachable says hangar answered the provider's last report.
	TargetReachable = "Reachable"
	// TargetReporting says the provider reported within its interval.
	TargetReporting = "Reporting"
)

// Report writes what the provider says of its hangar project into the
// Target's status: whether hangar answers, the project's room as its
// quota gives it (capacity: a project holds what its limits let it, not
// what a host has free), facts, last_report_time and Reporting, leaving
// what tower owns there as it is.
func (t *Target) Report(ctx context.Context) error {
	project, perr := t.api.Project(ctx)
	reach := &metav1.Condition{Type: TargetReachable, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Answered",
		Message: "hangar answered"}
	var capacity *targetsv1alpha1.Capacity
	facts := []*targetsv1alpha1.Fact{{Name: "provider", Value: "hangar"}}
	switch {
	case perr == nil:
		capacity, facts = quota(project, facts)
	case connect.CodeOf(perr) == connect.CodePermissionDenied || errors.Is(perr, ErrNotFound):
		// The token answers but may not read its project: its quota is not
		// known, and nothing says it is short of room.
		facts = append(facts, &targetsv1alpha1.Fact{Name: "quota", Value: "unknown: " + perr.Error()})
	default:
		reach.Status, reach.Reason, reach.Message = metav1.ConditionStatus_CONDITION_STATUS_FALSE, "NoAnswer", perr.Error()
	}
	if t.cfg.SystemImage != "" {
		ready, why, buildkit, tailnet := t.sys.get()
		v := "ready"
		if !ready {
			v = "not ready: " + why
		}
		facts = append(facts, &targetsv1alpha1.Fact{Name: "system container", Value: v})
		if ready {
			facts = append(facts, &targetsv1alpha1.Fact{Name: "builder", Value: or(buildkit, "buildkitd does not answer")},
				&targetsv1alpha1.Fact{Name: "builder on the tailnet", Value: strconv.FormatBool(tailnet)})
		}
		facts = append(facts, &targetsv1alpha1.Fact{Name: "mirror transport", Value: t.transportFact()},
			&targetsv1alpha1.Fact{Name: "mirror transfers", Value: t.transfersFact()})
	}
	for range 10 {
		o, err := t.c.Get(ctx, registry.Target, client.Key{Name: t.cfg.Target})
		if err != nil {
			return err
		}
		tg := o.(*targetsv1alpha1.Target)
		st := proto.Clone(tg.GetStatus()).(*targetsv1alpha1.TargetStatus)
		if st == nil {
			st = &targetsv1alpha1.TargetStatus{}
		}
		if capacity != nil {
			st.Capacity = capacity
		}
		st.Facts, st.LastReportTime = facts, timestamppb.New(t.clock.Now())
		now := t.clock.Now()
		st.Conditions = conditions.Set(slices.Clone(st.GetConditions()), reach, now)
		st.Conditions = conditions.Set(st.Conditions, &metav1.Condition{
			Type: TargetReporting, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Reported", Message: "the hangar provider reported",
		}, now)
		rv := tg.GetMetadata().GetResourceVersion()
		_, err = t.c.UpdateStatus(ctx, registry.Target, client.Key{Name: t.cfg.Target}, st, &rv)
		if connect.CodeOf(err) == connect.CodeAborted {
			// tower wrote what it owns in between: read it again.
			continue
		}
		return err
	}
	return errors.New("the target's status kept changing under its report")
}

// quota is the project's room, from its limits and usage, and facts with
// them added. A limit not set leaves its room unknown.
func quota(p *hangartenancy.Project, facts []*targetsv1alpha1.Fact) (*targetsv1alpha1.Capacity, []*targetsv1alpha1.Fact) {
	lim, use := p.GetStatus().GetLimits(), p.GetStatus().GetUsage()
	if lim == nil {
		lim = &hangartenancy.Limits{}
	}
	c := &targetsv1alpha1.Capacity{}
	if lim.Disk != nil {
		total, terr := quantity.Bytes(lim.GetDisk())
		used, uerr := quantity.Bytes(use.GetDisk())
		if terr == nil && (uerr == nil || use.GetDisk() == "") {
			c.DiskTotal = quantity.FormatDisk(total)
			c.DiskFree = quantity.FormatDisk(total - min(used, total))
		}
	}
	if lim.Memory != nil {
		total, terr := quantity.Bytes(lim.GetMemory())
		used, uerr := quantity.Bytes(use.GetMemory())
		if terr == nil && (uerr == nil || use.GetMemory() == "") {
			c.MemoryTotal = quantity.FormatMemory(total)
			c.MemoryFree = quantity.FormatMemory(total - min(used, total))
		}
	}
	if lim.Cpu != nil {
		if cpus, err := quantity.CPU(lim.GetCpu()); err == nil {
			c.Cpus = cpus
		}
	}
	count := func(name string, used int32, limit *int32) {
		v := strconv.Itoa(int(used))
		if limit != nil {
			v += " of " + strconv.Itoa(int(*limit))
		}
		facts = append(facts, &targetsv1alpha1.Fact{Name: name, Value: v})
	}
	count("hangar machines", use.GetMachines(), lim.Machines)
	count("hangar networks", use.GetNetworks(), lim.Networks)
	count("hangar custom images", use.GetCustomImages(), lim.CustomImages)
	count("hangar network endpoints", use.GetNetworkEndpoints(), lim.NetworkEndpoints)
	facts = append(facts, &targetsv1alpha1.Fact{Name: "hangar disk", Value: fmt.Sprintf("%s of %s", or(use.GetDisk(), "0"), or(lim.GetDisk(), "unlimited"))})
	return c, facts
}

func or(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
