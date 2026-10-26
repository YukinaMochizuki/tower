package apiserver

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/tower/internal/controller"
)

// TargetDiskPressure says a target has less disk free than its warm budget
// and a margin need. tower's controllers own it, counts and usage of
// goldens and instances, last_error, and a hostd target's Reachable
// (reachable); the provider owns the rest of TargetStatus (capacity,
// facts, a hangar target's Reachable, Reporting, the other usage), which
// they leave as it is, but for Reporting turned false once the provider
// has been silent for ReportsMissed report intervals.
const TargetDiskPressure = "DiskPressure"

// TargetReporting says the provider reported within its interval.
const TargetReporting = "Reporting"

// jobErrors are the last failure of a job on each target, until a job on
// it succeeds.
type jobErrors struct {
	mu   sync.Mutex
	last map[string]jobError
}

type jobError struct {
	at  time.Time
	job string
	err string
}

// startJob starts a job of a controller's, as jobs.start does, on target:
// its failure is the target's last_error until a job on the target
// succeeds.
func (s *Server) startJob(target, name string, ctrl *controller.Controller, key controller.Key, fn func(ctx context.Context) error) {
	s.jobs.start(s.runCtx, name, ctrl, key, func(ctx context.Context) error {
		err := fn(ctx)
		if ctx.Err() == nil {
			s.jobEnded(target, name, err)
		}
		return err
	})
}

func (s *Server) jobEnded(target, name string, err error) {
	s.jobErrs.mu.Lock()
	defer s.jobErrs.mu.Unlock()
	if s.jobErrs.last == nil {
		s.jobErrs.last = map[string]jobError{}
	}
	_, had := s.jobErrs.last[target]
	switch {
	case err != nil:
		s.jobErrs.last[target] = jobError{at: s.clock.Now(), job: name, err: err.Error()}
	case had:
		delete(s.jobErrs.last, target)
	default:
		return
	}
	if s.targetCtrl != nil {
		s.targetCtrl.Enqueue(controller.Key{Name: target})
	}
}

// lastJobError is what last_error says of the target's jobs, "" while the
// last one succeeded.
func (s *Server) lastJobError(target string) string {
	s.jobErrs.mu.Lock()
	defer s.jobErrs.mu.Unlock()
	e, ok := s.jobErrs.last[target]
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s: %s: %s", e.at.UTC().Format(time.RFC3339), e.job, e.err)
}

// silent sets Reporting false in st once the target's provider has not
// reported for ReportsMissed intervals, counted from its last report or,
// with none, from when the target was made.
func (s *Server) silent(t *targetsv1alpha1.Target, st *targetsv1alpha1.TargetStatus) {
	since := t.GetMetadata().GetCreateTime().AsTime()
	if r := st.GetLastReportTime(); r != nil {
		since = r.AsTime()
	}
	if s.clock.Since(since) < ReportsMissed*DefaultReportInterval {
		return
	}
	msg := "the provider has never reported"
	if st.GetLastReportTime() != nil {
		msg = "the provider has not reported since " + since.UTC().Format(time.RFC3339)
	}
	st.Conditions = conditions.Set(slices.Clone(st.GetConditions()), &metav1.Condition{
		Type: TargetReporting, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: "Silent", Message: msg,
	}, s.clock.Now())
}

// reconcileTargetStatus counts what tower holds on the target into its
// status.
func (s *Server) reconcileTargetStatus(ctx context.Context, key controller.Key) error {
	t, live, err := s.liveTarget(ctx, key.Name)
	if err != nil || !live {
		return err
	}
	instances, err := listTyped[*envv1alpha1.Instance](ctx, s, registry.Instance, "")
	if err != nil {
		return err
	}
	goldens, err := listTyped[*envv1alpha1.Golden](ctx, s, registry.Golden, "")
	if err != nil {
		return err
	}
	counts := &targetsv1alpha1.Counts{Instances: map[string]int32{}}
	var instanceDisk, goldenDisk uint64
	for _, in := range instances {
		if in.GetSpec().GetTarget() != key.Name {
			continue
		}
		phase := strings.TrimPrefix(in.GetStatus().GetPhase().String(), "INSTANCE_PHASE_")
		counts.Instances[phase]++
		if b, err := quantity.Bytes(in.GetStatus().GetUsedDisk()); err == nil {
			instanceDisk += b
		}
	}
	for _, g := range goldens {
		if g.GetSpec().GetTarget() != key.Name || g.GetMetadata().GetDeleteTime() != nil {
			continue
		}
		counts.Goldens++
		if b, err := quantity.Bytes(g.GetStatus().GetSize()); err == nil {
			goldenDisk += b
		}
	}
	st := proto.Clone(t.GetStatus()).(*targetsv1alpha1.TargetStatus)
	if st == nil {
		st = &targetsv1alpha1.TargetStatus{}
	}
	st.ObservedGeneration = t.GetMetadata().GetGeneration()
	st.Counts = counts
	if st.Usage == nil {
		st.Usage = &targetsv1alpha1.Usage{}
	}
	st.Usage.Goldens, st.Usage.Instances = quantity.FormatDisk(goldenDisk), quantity.FormatDisk(instanceDisk)
	st.LastError = s.lastJobError(key.Name)
	// Only a provider of tower's own follows the tunnel: one given in
	// Config says itself whether it answers.
	if t.GetSpec().GetHostd() != nil && s.cfg.Providers == nil {
		s.reachable(key.Name, st)
	}
	if t.GetSpec().GetHangar() != nil && s.cfg.Providers == nil {
		s.hangarReachable(key.Name, st)
	}
	s.silent(t, st)
	if free, err := quantity.Bytes(st.GetCapacity().GetDiskFree()); err == nil && st.GetCapacity().GetDiskFree() != "" {
		need := settingsOf(t).warmBudget + mustBytes(DiskPressureMargin)
		c := &metav1.Condition{Type: TargetDiskPressure, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: "EnoughFree",
			Message: fmt.Sprintf("%s free, %s needed", quantity.FormatDisk(free), quantity.FormatDisk(need))}
		if free < need {
			c.Status, c.Reason = metav1.ConditionStatus_CONDITION_STATUS_TRUE, "LowOnDisk"
		}
		st.Conditions = conditions.Set(slices.Clone(st.GetConditions()), c, s.clock.Now())
	}
	return s.updateStatus(ctx, registry.Target, t, st)
}
