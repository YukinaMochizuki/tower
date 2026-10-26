package cli

import (
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1/environmentsv1alpha1connect"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
)

// retryCmd is "retry": a FAILED object's failed work, run now.
func (a *app) retryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "retry golden|box|machine NAME",
		Short: "Run a FAILED golden's, box's or machine's failed work now",
		Long: `Run the failed work of a FAILED golden, box or machine now, instead of when
its backoff runs out: once the cause of a failure is fixed outside tower (a
registry back, a repository pushed). The failures counted against the work
are forgotten, so a failure again waits out the first backoff. Of a box or
a machine, its golden is built again if that failed, and its failed steps
run again. A box that fell back to its fallback image because its
devcontainer's image did not build (its DevcontainerFellBack condition
names the golden) has that golden built again now if it is FAILED, the box
READY or not: a box made after is made of the devcontainer's image once it
builds, while this box stays on the image it fell back to. An object that
is neither is left as it is. retry does not wait: follow the object with
tower get or tower wait.`,
		Example: "  tower retry box dev -p web\n" +
			"  tower retry golden img-0123456789abcdef -p web",
	}
	cmd.AddCommand(
		a.retryKindCmd(registry.Golden, func(ctx context.Context, c retrier, project, name string) (proto.Message, bool, error) {
			res, err := c.RetryGolden(ctx, connect.NewRequest(&envv1alpha1.RetryGoldenRequest{Project: project, Name: name}))
			if err != nil {
				return nil, false, err
			}
			return res.Msg.GetGolden(), res.Msg.GetRetried(), nil
		}),
		a.retryKindCmd(registry.Box, func(ctx context.Context, c retrier, project, name string) (proto.Message, bool, error) {
			res, err := c.RetryBox(ctx, connect.NewRequest(&envv1alpha1.RetryBoxRequest{Project: project, Name: name}))
			if err != nil {
				return nil, false, err
			}
			return res.Msg.GetBox(), res.Msg.GetRetried(), nil
		}),
		a.retryKindCmd(registry.Machine, func(ctx context.Context, c retrier, project, name string) (proto.Message, bool, error) {
			res, err := c.RetryMachine(ctx, connect.NewRequest(&envv1alpha1.RetryMachineRequest{Project: project, Name: name}))
			if err != nil {
				return nil, false, err
			}
			return res.Msg.GetMachine(), res.Msg.GetRetried(), nil
		}),
	)
	return cmd
}

type retrier = environmentsv1alpha1connect.RetryServiceClient

// retryKindCmd is "retry KIND NAME", calling call.
func (a *app) retryKindCmd(r registry.Resource, call func(ctx context.Context, c retrier, project, name string) (proto.Message, bool, error)) *cobra.Command {
	kind := strings.ToLower(r.Kind)
	return &cobra.Command{
		Use:   kind + " NAME",
		Short: "Run a FAILED " + kind + "'s failed work now",
		Args:  args(1, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			project, err := a.projectFor(r)
			if err != nil {
				return err
			}
			hc, base, err := a.endpoint()
			if err != nil {
				return err
			}
			obj, retried, err := call(cmd.Context(), environmentsv1alpha1connect.NewRetryServiceClient(hc, base), project, args[0])
			if err != nil {
				return err
			}
			if retried {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s retried: its failed work runs now\n", objectName(r, obj))
			} else if b, ok := obj.(*envv1alpha1.Box); ok && b.GetStatus().GetDevcontainerGolden() != "" {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s fell back from golden %s, which is not FAILED: nothing to retry\n", objectName(r, obj), b.GetStatus().GetDevcontainerGolden())
			} else {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s is %s, not FAILED: nothing to retry\n", objectName(r, obj), phaseColumn(obj))
			}
			return nil
		},
	}
}
