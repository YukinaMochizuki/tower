package apiserver

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/provider"
)

// The inotify limits a box or boost on a hangar target runs with. Its
// guest's kernel sizes max_user_watches from its memory, some 16k in a
// 2Gi box: fewer than the directories of one /workspace with its caches,
// so a dev server watching it fails with ENOSPC, read as a full disk. A
// watch costs kernel memory, about 1KB, only once it is used. On a hostd
// target the kernel is the host's, which tower leaves as it is.
const (
	inotifyMaxUserWatches   = 524288
	inotifyMaxUserInstances = 1024
)

const (
	// BoxKernelLimits and BoostKernelLimits, on a hangar target, say
	// whether its instance's kernel limits were raised in this run of it.
	// One that was not leaves it READY, saying why here.
	BoxKernelLimits   = "KernelLimits"
	BoostKernelLimits = "KernelLimits"
	// kernelLimitsAnnotation is the start of an instance's run its kernel
	// limits were raised in, or failed to be (runOf): sysctls do not
	// outlive a start.
	kernelLimitsAnnotation = registry.Domain + "/kernel-limits"
	// kernelLimitsWarningAnnotation is why they were not, if they were not.
	kernelLimitsWarningAnnotation = registry.Domain + "/kernel-limits-warning"
)

// raiseKernelLimits is the command, run as root, that raises each limit
// to its value, and lowers none a kernel has higher.
var raiseKernelLimits = []string{"sh", "-c", `set -e
raise() { f=/proc/sys/fs/inotify/$1; [ "$(cat "$f")" -ge "$2" ] || echo "$2" > "$f"; }
raise max_user_watches ` + strconv.Itoa(inotifyMaxUserWatches) + `
raise max_user_instances ` + strconv.Itoa(inotifyMaxUserInstances)}

// raisesKernelLimits reports whether tower raises the kernel limits of
// instances on target: a hangar target's, whose guests have kernels of
// their own.
func raisesKernelLimits(target *targetsv1alpha1.Target) bool {
	return providerOf(target) == "hangar"
}

// kernelLimitsDue reports whether in's kernel limits are to be raised in
// this run of it.
func kernelLimitsDue(target *targetsv1alpha1.Target, in *envv1alpha1.Instance) bool {
	return raisesKernelLimits(target) && in.GetMetadata().GetAnnotations()[kernelLimitsAnnotation] != runOf(in)
}

// kernelLimitsCondition is the KernelLimits condition of an instance whose
// limits were raised, or failed to be, in this run: its status, reason and
// message.
func kernelLimitsCondition(in *envv1alpha1.Instance) (bool, string, string) {
	if w := in.GetMetadata().GetAnnotations()[kernelLimitsWarningAnnotation]; w != "" {
		return false, "NotRaised", w
	}
	return true, "Raised", ""
}

// raiseKernelLimitsIn raises ref's kernel limits, then marks it with run,
// the run they were raised in. A failure of the work fails nothing: it
// is kept on the instance for its condition, and the next start tries
// again. Only a target that did not answer is an error, to wait on. A job
// started on an instance read before an earlier one marked it finds the
// work done.
func (s *Server) raiseKernelLimitsIn(ctx context.Context, p provider.Provider, project string, ref provider.Instance, run string) error {
	in, ok, err := getTyped[*envv1alpha1.Instance](ctx, s, registry.Instance, resource.Key{Project: project, Name: ref.Name})
	if err != nil || !ok || in.GetMetadata().GetAnnotations()[kernelLimitsAnnotation] == run {
		return err
	}
	var out bytes.Buffer
	code, err := p.Exec(ctx, provider.ExecRequest{Instance: ref, Command: raiseKernelLimits, Dir: "/", AsRoot: true, Stdout: &out, Stderr: &out})
	var warning string
	switch {
	case transient(err) || ctx.Err() != nil:
		return err
	case err != nil:
		warning = fmt.Sprintf("raising the inotify limits: %v", err)
	case code != 0:
		tail := strings.TrimSpace(out.String())
		if len(tail) > 300 {
			tail = tail[len(tail)-300:]
		}
		warning = fmt.Sprintf("raising the inotify limits: exit %d: %s", code, tail)
	}
	if warning != "" {
		s.log.Warn("an instance's kernel limits were not raised", "project", project, "instance", ref.Name, "reason", warning)
	}
	if err := s.annotateInstance(ctx, project, ref.Name, kernelLimitsWarningAnnotation, warning); err != nil {
		return err
	}
	return s.annotateInstance(ctx, project, ref.Name, kernelLimitsAnnotation, run)
}
