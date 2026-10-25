package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/durationpb"

	iamv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1/iamv1alpha1connect"
)

// tokenCmd is "token": service tokens' secrets.
func (a *app) tokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Create service tokens and rotate their secrets",
		Long: `Create service tokens and rotate their secrets. A token's role says what it
may do: admin (tower itself), workstream (intents and the environments'
data plane), or hostd (one target's host; --target names it). Its secret is
shown once, by create and rotate, and never again: tower keeps only its
hash. Only an admin token, or root on tower's host through the admin
socket, may create or rotate one. Delete a token with tower delete
servicetoken NAME.`,
	}
	cmd.AddCommand(a.tokenCreateCmd(), a.tokenRotateCmd())
	return cmd
}

func (a *app) issuer() (iamv1alpha1connect.ServiceTokenIssuerServiceClient, error) {
	hc, base, err := a.endpoint()
	if err != nil {
		return nil, err
	}
	return iamv1alpha1connect.NewServiceTokenIssuerServiceClient(hc, base), nil
}

// secretOut is where a new secret goes: a file made for it, or stdout.
type secretOut struct {
	path string
	f    *os.File
}

// openSecretOut makes the file the secret goes to, if there is one, before
// anything is issued, so a secret is never issued with nowhere to go. It
// refuses a file that exists.
func openSecretOut(path string) (*secretOut, error) {
	if path == "" {
		return &secretOut{}, nil
	}
	f, err := createPrivate(path, true)
	if errors.Is(err, os.ErrExist) {
		return nil, usageErr("%s exists; the secret goes only to a new file", path)
	}
	if err != nil {
		return nil, err
	}
	return &secretOut{path: path, f: f}, nil
}

// abandon removes the file when nothing was issued.
func (o *secretOut) abandon() {
	if o.f != nil {
		_ = o.f.Close()
		_ = os.Remove(o.path)
	}
}

// write puts the secret where it goes, and what was done on the other
// stream: with a file, stdout says it; without, stdout holds the secret
// alone, for a script to take, and stderr says the rest.
func (o *secretOut) write(cmd *cobra.Command, secret, what string) error {
	if o.f == nil {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), secret); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s; the secret above is shown only now\n", what)
		return nil
	}
	_, err := fmt.Fprintln(o.f, secret)
	if cerr := o.f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("writing the secret to %s: %w; rotate the token to issue another", o.path, err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s; the secret is in %s\n", what, o.path)
	return nil
}

func durationFlag(d time.Duration) *durationpb.Duration {
	if d == 0 {
		return nil
	}
	return durationpb.New(d)
}

func describeIssued(t *iamv1alpha1.ServiceToken) string {
	return fmt.Sprintf("service token %s (%s…), expires %s", t.GetMetadata().GetName(),
		t.GetStatus().GetSecretPrefix(), t.GetStatus().GetExpireTime().AsTime().UTC().Format(time.RFC3339))
}

// parseRole reads a role by its short name, such as workstream.
func parseRole(s string) (iamv1alpha1.Role, error) {
	v, ok := iamv1alpha1.Role_value["ROLE_"+strings.ToUpper(s)]
	if !ok || v == 0 {
		return 0, usageErr("unknown role %q; want admin, workstream or hostd", s)
	}
	return iamv1alpha1.Role(v), nil
}

func (a *app) tokenCreateCmd() *cobra.Command {
	var role, target, description, secretFile string
	var duration time.Duration
	cmd := &cobra.Command{
		Use:   "create NAME --role ROLE [--target T] [--duration D] [--secret-file FILE]",
		Short: "Create a service token, and print its secret once",
		Long: `Create a service token and print its secret, once, alone on stdout, or write
it to --secret-file, a new file of mode 0600, and print nothing of it. The
secret lasts --duration: a year unless set, at least 1h, at most three
years.`,
		Example: "  tower token create workstream --role workstream --secret-file workstream.token\n" +
			"  tower token create hostd-main --role hostd --target main --secret-file hostd.token",
		Args: args(1, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := parseRole(role)
			if err != nil {
				return err
			}
			issuer, err := a.issuer()
			if err != nil {
				return err
			}
			out, err := openSecretOut(secretFile)
			if err != nil {
				return err
			}
			res, err := issuer.CreateServiceToken(cmd.Context(), connect.NewRequest(&iamv1alpha1.CreateServiceTokenRequest{
				Name: args[0], Description: description, Role: r, Target: target, Duration: durationFlag(duration),
			}))
			if err != nil {
				out.abandon()
				return err
			}
			return out.write(cmd, res.Msg.GetSecret(), "created "+describeIssued(res.Msg.GetServiceToken()))
		},
	}
	cmd.Flags().StringVar(&role, "role", "", "admin, workstream or hostd")
	cmd.Flags().StringVar(&target, "target", "", "the Target a hostd token acts for")
	cmd.Flags().DurationVar(&duration, "duration", 0, "how long the secret lasts, such as 720h; a year unless set")
	cmd.Flags().StringVar(&description, "description", "", "what the token is for")
	cmd.Flags().StringVar(&secretFile, "secret-file", "", "new file, mode 0600, to write the secret to instead of stdout")
	return cmd
}

func (a *app) tokenRotateCmd() *cobra.Command {
	var secretFile string
	var duration time.Duration
	cmd := &cobra.Command{
		Use:   "rotate NAME [--duration D] [--secret-file FILE]",
		Short: "Issue a service token a new secret; the old one stops working at once",
		Long: `Issue the token a new secret, printed once as create prints it. The secret it
had stops working the moment the new one is issued.`,
		Args: args(1, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			issuer, err := a.issuer()
			if err != nil {
				return err
			}
			out, err := openSecretOut(secretFile)
			if err != nil {
				return err
			}
			res, err := issuer.RotateServiceToken(cmd.Context(), connect.NewRequest(&iamv1alpha1.RotateServiceTokenRequest{
				Name: args[0], Duration: durationFlag(duration),
			}))
			if err != nil {
				out.abandon()
				return err
			}
			return out.write(cmd, res.Msg.GetSecret(), "rotated "+describeIssued(res.Msg.GetServiceToken()))
		},
	}
	cmd.Flags().DurationVar(&duration, "duration", 0, "how long the new secret lasts; a year unless set")
	cmd.Flags().StringVar(&secretFile, "secret-file", "", "new file, mode 0600, to write the secret to instead of stdout")
	return cmd
}
