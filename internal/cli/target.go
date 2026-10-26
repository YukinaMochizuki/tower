package cli

import (
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1/targetsv1alpha1connect"
)

// targetCmd is "target": what is done to a Target beyond apply, get and
// delete.
func (a *app) targetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "target",
		Short: "Act on a target beyond apply, get and delete",
	}
	cmd.AddCommand(a.targetAbandonCmd())
	return cmd
}

// targetAbandonCmd is "target abandon NAME".
func (a *app) targetAbandonCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "abandon NAME",
		Short: "Let a target being deleted go without its teardown done",
		Long: `Let a target being deleted go before its teardown is done: for a target
that cannot answer, such as a hangar project whose token is refused, or a
host gone for good. Deleting a target first deletes what tower made on it,
and the target stays, being deleted, until nothing is left; its status's
remaining says what is, and last_error what keeps it. Abandoning it ends
that wait. What its teardown last found left is printed, and may still be
on the target, counting against its quota: delete it there by hand. A
teardown that never listed the target, as one refused from the start,
leaves what is on it unknown, and says so.
Admin only (an admin token, or root on tower's host); a target that is
not being deleted is refused.`,
		Example: "  tower delete target h1\n" +
			"  tower get target h1 -o yaml   # status.remaining, status.last_error\n" +
			"  tower target abandon h1",
		Args: args(1, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, base, err := a.endpoint()
			if err != nil {
				return err
			}
			res, err := targetsv1alpha1connect.NewTargetActionServiceClient(hc, base).AbandonTarget(cmd.Context(),
				connect.NewRequest(&targetsv1alpha1.AbandonTargetRequest{Name: args[0]}))
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "target/%s abandoned: its teardown was not confirmed\n", args[0])
			left := res.Msg.GetUnconfirmed()
			known := res.Msg.GetUnconfirmedKnown()
			switch {
			case !known:
				_, _ = fmt.Fprintln(out, "its teardown never listed the target: what is left on it is unknown")
			case len(left) == 0:
				_, _ = fmt.Fprintln(out, "its teardown last found nothing left")
			}
			if len(left) == 0 {
				return nil
			}
			_, _ = fmt.Fprintln(out, "its teardown last found left, which may still be on the target:")
			for _, l := range left {
				_, _ = fmt.Fprintf(out, "  %s\n", l)
			}
			return nil
		},
	}
}
