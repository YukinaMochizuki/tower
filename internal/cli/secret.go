package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	secretsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/secrets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/secrets"
)

// secretCmd is "secret": Secrets, whose values never come from the
// command line.
func (a *app) secretCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "secret", Short: "Set secrets; their values are read from files or stdin, never from the command line"}
	cmd.AddCommand(a.secretSetCmd())
	return cmd
}

func (a *app) secretSetCmd() *cobra.Command {
	var fromFile []string
	var fromStdin, typ string
	var createOnly bool
	cmd := &cobra.Command{
		Use:   "set NAME (--from-file KEY=PATH... | --from-stdin KEY) [--type opaque|registry-auth]",
		Short: "Create or replace a secret, with every value it holds",
		Long: `Create or replace a secret in the project with the values given, which are
all it holds after: each --from-file KEY=PATH reads KEY's value from a
file, and --from-stdin KEY reads one key's from stdin. An opaque secret's
keys are environment variable names; a registry-auth secret holds one key,
.dockerconfigjson, in the Docker config format. Values are write-only:
nothing shows them again.

With --create-only, a secret whose name exists is refused and left as it is.`,
		Example: "  tower secret set aws -p web --from-file AWS_ACCESS_KEY_ID=key-id --from-file AWS_SECRET_ACCESS_KEY=key\n" +
			"  pass show forge-token | tower secret set forge -p web --from-stdin token",
		Args: args(1, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(fromFile) == 0 && fromStdin == "" {
				return usageErr("give the values with --from-file KEY=PATH or --from-stdin KEY")
			}
			st, err := secretType(typ)
			if err != nil {
				return err
			}
			data := map[string][]byte{}
			// stdin is read for one key at most: by --from-stdin or a
			// --from-file of "-".
			stdinFor := fromStdin
			for _, kv := range fromFile {
				if k, p, _ := strings.Cut(kv, "="); p == "-" {
					if stdinFor != "" {
						return usageErr("stdin holds one value; %q and %q both ask for it", stdinFor, k)
					}
					stdinFor = k
				}
			}
			for _, kv := range fromFile {
				k, p, ok := strings.Cut(kv, "=")
				if !ok || k == "" || p == "" {
					return usageErr("--from-file takes KEY=PATH, not %q", kv)
				}
				if _, dup := data[k]; dup {
					return usageErr("key %q is given twice", k)
				}
				if data[k], err = readFileOrStdin(p, cmd.InOrStdin()); err != nil {
					return err
				}
			}
			if fromStdin != "" {
				if _, dup := data[fromStdin]; dup {
					return usageErr("key %q is given twice", fromStdin)
				}
				if data[fromStdin], err = readFileOrStdin("-", cmd.InOrStdin()); err != nil {
					return err
				}
			}
			// Checked here too, so bad values never leave the machine.
			if err := secrets.Validate(st, data); err != nil {
				return usageErr("%v", err)
			}
			cl, err := a.client()
			if err != nil {
				return err
			}
			sec := &secretsv1alpha1.Secret{
				Metadata: &metav1.ObjectMeta{Name: args[0]},
				Spec:     &secretsv1alpha1.SecretSpec{Type: st, Data: data},
			}
			if sec.Metadata.Project, err = a.projectFor(registry.Secret); err != nil {
				return err
			}
			apply := cl.Apply
			if createOnly {
				apply = cl.Create
			}
			res, err := apply(cmd.Context(), registry.Secret, sec)
			if createOnly && connect.CodeOf(err) == connect.CodeAlreadyExists {
				return &exitError{code: ExitConflict, err: fmt.Errorf("secret/%s already exists, and --create-only leaves it as it is", args[0])}
			}
			if err != nil {
				return err
			}
			verb := "applied"
			if res.Created {
				verb = "created"
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s %s, with %d keys\n", objectName(registry.Secret, res.Object), verb, len(data))
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&fromFile, "from-file", nil, "KEY=PATH: a key and the file holding its value; repeatable")
	cmd.Flags().StringVar(&fromStdin, "from-stdin", "", "a key whose value is read from stdin")
	cmd.Flags().StringVar(&typ, "type", "opaque", "opaque or registry-auth")
	cmd.Flags().BoolVar(&createOnly, "create-only", false, "refuse a secret that exists instead of replacing it")
	return cmd
}

func secretType(s string) (secretsv1alpha1.SecretType, error) {
	switch s {
	case "opaque":
		return secretsv1alpha1.SecretType_SECRET_TYPE_OPAQUE, nil
	case "registry-auth":
		return secretsv1alpha1.SecretType_SECRET_TYPE_REGISTRY_AUTH, nil
	}
	return 0, usageErr("unknown secret type %q; want opaque or registry-auth", s)
}

// readFileOrStdin reads a file, or stdin for "-", up to a bound past what
// a secret may hold.
func readFileOrStdin(path string, stdin io.Reader) ([]byte, error) {
	r := stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, secrets.MaxDataSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > secrets.MaxDataSize {
		return nil, usageErr("%s is larger than a secret may hold (%d bytes)", path, secrets.MaxDataSize)
	}
	return b, nil
}
