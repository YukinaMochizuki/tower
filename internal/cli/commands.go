package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/object"
	"github.com/YukinaMochizuki/tower/internal/client"
)

func (a *app) applyCmd() *cobra.Command {
	var files []string
	var output string
	var createOnly bool
	cmd := &cobra.Command{
		Use:   "apply -f FILE|DIR|-",
		Short: "Create or replace the objects in YAML or JSON files",
		Long: `Create or replace the objects in YAML or JSON files, several documents to a
file, in order. A project-scoped object with no metadata.project goes in
--project's.

A number written where the API has a string is sent as the text written, so
"cpu: 2" needs no quotes, and 1.10 stays 1.10.

With --create-only, an object whose name exists is refused and left as it
is, and apply stops there; the objects before it stay created.`,
		Args: args(0, 0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(files) == 0 {
				return usageErr("-f is required")
			}
			if output != "" && output != "name" && output != "json" && output != "yaml" {
				return usageErr("apply prints name, json or yaml, not %q", output)
			}
			manifests, err := readManifests(files, cmd.InOrStdin())
			if err != nil {
				return err
			}
			cl, err := a.client()
			if err != nil {
				return err
			}
			var applied []proto.Message
			for _, m := range manifests {
				meta := object.Meta(m.obj)
				if m.r.ProjectScoped() && meta.GetProject() == "" {
					if meta.Project, err = a.projectFor(m.r); err != nil {
						return fmt.Errorf("%s: %w", m.source, err)
					}
				}
				apply := cl.Apply
				if createOnly {
					apply = cl.Create
				}
				res, err := apply(cmd.Context(), m.r, m.obj)
				if err != nil {
					return fmt.Errorf("%s: %w", m.source, err)
				}
				for _, w := range res.Warnings {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s: %s\n", objectName(m.r, res.Object), w)
				}
				switch output {
				case "json", "yaml":
					applied = append(applied, res.Object)
				case "name":
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), objectName(m.r, res.Object))
				default:
					verb := "applied"
					if res.Created {
						verb = "created"
					}
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", objectName(m.r, res.Object), verb)
				}
			}
			if output == "json" || output == "yaml" {
				return writeDocument(cmd.OutOrStdout(), output, applied, false)
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVarP(&files, "filename", "f", nil, "file, directory or - for stdin; repeatable")
	cmd.Flags().StringVarP(&output, "output", "o", "", "name, json or yaml")
	cmd.Flags().BoolVar(&createOnly, "create-only", false, "refuse objects that exist instead of replacing them")
	return cmd
}

func (a *app) getCmd() *cobra.Command {
	var output, selector string
	var all bool
	cmd := &cobra.Command{
		Use:   "get KIND [NAME...]",
		Short: "Show objects of a kind",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usageErr("usage: %s\n%s", cmd.UseLine(), knownKinds())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if output == "" {
				output = "table"
			}
			if err := checkOutput(output); err != nil {
				return err
			}
			r, err := findKind(args[0])
			if err != nil {
				return err
			}
			cl, err := a.client()
			if err != nil {
				return err
			}
			var project string
			acrossProjects := all && r.ProjectScoped()
			if !acrossProjects {
				if project, err = a.projectFor(r); err != nil {
					return err
				}
			}
			var objs []proto.Message
			if names := args[1:]; len(names) > 0 {
				for _, n := range names {
					o, err := cl.Get(cmd.Context(), r, client.Key{Project: project, Name: n})
					if err != nil {
						return err
					}
					objs = append(objs, o)
				}
				return a.printObjects(cmd.OutOrStdout(), output, r, objs, len(names) == 1, false)
			}
			objs, _, err = cl.ListAll(cmd.Context(), r, client.ListOptions{Project: project, LabelSelector: selector})
			if err != nil {
				return err
			}
			return a.printObjects(cmd.OutOrStdout(), output, r, objs, false, all)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "table, json, yaml or name")
	cmd.Flags().StringVarP(&selector, "selector", "l", "", "label selector, such as app=web,tier!=db")
	cmd.Flags().BoolVarP(&all, "all-projects", "A", false, "list across every project you can read")
	return cmd
}

func (a *app) deleteCmd() *cobra.Command {
	var files []string
	var output string
	cmd := &cobra.Command{
		Use:   "delete KIND NAME... | delete -f FILE",
		Short: "Delete objects",
		Args:  args(0, -1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "" && output != "name" {
				return usageErr("delete prints name, not %q", output)
			}
			cl, err := a.client()
			if err != nil {
				return err
			}
			type target struct {
				r   registry.Resource
				key client.Key
			}
			var targets []target
			switch {
			case len(files) > 0 && len(args) == 0:
				manifests, err := readManifests(files, cmd.InOrStdin())
				if err != nil {
					return err
				}
				for _, m := range manifests {
					meta := object.Meta(m.obj)
					key := client.Key{Project: meta.GetProject(), Name: meta.GetName()}
					if m.r.ProjectScoped() && key.Project == "" {
						if key.Project, err = a.projectFor(m.r); err != nil {
							return err
						}
					}
					targets = append(targets, target{m.r, key})
				}
			case len(files) == 0 && len(args) >= 2:
				r, err := findKind(args[0])
				if err != nil {
					return err
				}
				project, err := a.projectFor(r)
				if err != nil {
					return err
				}
				for _, n := range args[1:] {
					targets = append(targets, target{r, client.Key{Project: project, Name: n}})
				}
			default:
				return usageErr("usage: %s", cmd.UseLine())
			}
			fromFiles := len(files) > 0
			if fromFiles {
				// Files list owners before what they own; deleting in reverse
				// removes contents first, as a cascade from the owner would.
				slices.Reverse(targets)
			}
			for _, t := range targets {
				o, err := cl.Delete(cmd.Context(), t.r, t.key)
				// An object in a file may already be gone, taken by the
				// cascade from an owner deleted before it; that is the
				// outcome asked for.
				if fromFiles && connect.CodeOf(err) == connect.CodeNotFound {
					name := strings.ToLower(t.r.Kind) + "/" + t.key.Name
					if output == "name" {
						_, _ = fmt.Fprintln(cmd.OutOrStdout(), name)
					} else {
						_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s already deleted\n", name)
					}
					continue
				}
				if err != nil {
					return err
				}
				if output == "name" {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), objectName(t.r, o))
				} else {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s deleted\n", objectName(t.r, o))
				}
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVarP(&files, "filename", "f", nil, "file, directory or - for stdin; repeatable")
	cmd.Flags().StringVarP(&output, "output", "o", "", "name")
	return cmd
}

// waitFor is what wait waits for.
type waitFor struct {
	// condition is a condition's type to be True for the current
	// generation; with neither it nor deleted, the status's
	// observed_generation catching up.
	condition string
	deleted   bool
}

func parseWaitFor(s string) (waitFor, error) {
	switch {
	case s == "delete":
		return waitFor{deleted: true}, nil
	case s == "observed":
		return waitFor{}, nil
	case strings.HasPrefix(s, "condition=") && len(s) > len("condition="):
		return waitFor{condition: strings.TrimPrefix(s, "condition=")}, nil
	}
	return waitFor{}, usageErr("--for must be condition=TYPE, observed or delete, not %q", s)
}

func (a *app) waitCmd() *cobra.Command {
	var forCond string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "wait KIND NAME --for=condition=TYPE|observed|delete",
		Short: "Wait until an object meets a condition, is observed, or is deleted",
		Long: `Wait until an object meets a condition, is observed, or is deleted.

condition=TYPE waits until the condition is True for the object's current
generation, so a change just applied has taken effect. observed waits until
its status's observed_generation has caught up with its generation. delete
waits until it is gone.`,
		Args: args(2, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			w, err := parseWaitFor(forCond)
			if err != nil {
				return err
			}
			r, err := findKind(args[0])
			if err != nil {
				return err
			}
			cl, err := a.client()
			if err != nil {
				return err
			}
			project, err := a.projectFor(r)
			if err != nil {
				return err
			}
			return await(cmd.Context(), cl, r, client.Key{Project: project, Name: args[1]}, w, timeout)
		},
	}
	cmd.Flags().StringVar(&forCond, "for", "observed", "condition=TYPE, observed or delete")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "give up after this long")
	return cmd
}

// await waits until the object meets w.
func await(ctx context.Context, cl *client.Client, r registry.Resource, key client.Key, w waitFor, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := func(o proto.Message) bool {
		switch {
		case w.deleted:
			return o == nil
		case o == nil:
			return false
		case w.condition != "":
			return conditionMet(o, w.condition)
		}
		return statusObservedGeneration(o) >= object.Meta(o).GetGeneration()
	}

	o, err := cl.Get(ctx, r, key)
	switch {
	case connect.CodeOf(err) == connect.CodeNotFound && w.deleted:
		return nil
	case err != nil:
		return timeoutOr(ctx, err)
	case done(o):
		return nil
	}
	errDone := errors.New("done")
	err = cl.Watch(ctx, r, client.ListOptions{Project: key.Project}, object.Meta(o).GetResourceVersion(), func(ev client.Event) error {
		if ev.Object == nil || object.Meta(ev.Object).GetName() != key.Name {
			return nil
		}
		if ev.Type == metav1.EventType_EVENT_TYPE_DELETED {
			if w.deleted {
				return errDone
			}
			return &exitError{code: ExitNotFound, err: fmt.Errorf("%s was deleted", objectName(r, ev.Object))}
		}
		if done(ev.Object) {
			return errDone
		}
		return nil
	})
	if errors.Is(err, errDone) {
		return nil
	}
	return timeoutOr(ctx, err)
}

// conditionMet reports whether the condition is True and was computed for the
// object's current generation.
func conditionMet(o proto.Message, typ string) bool {
	c := condition(o, typ)
	if c == nil || c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE {
		return false
	}
	observed := c.GetObservedGeneration()
	if observed == 0 {
		observed = statusObservedGeneration(o)
	}
	return observed >= object.Meta(o).GetGeneration()
}

func statusObservedGeneration(o proto.Message) int64 {
	st := object.Status(o).ProtoReflect()
	fd := st.Descriptor().Fields().ByName("observed_generation")
	if fd == nil {
		return 0
	}
	return st.Get(fd).Int()
}

func timeoutOr(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &exitError{code: ExitTimeout, err: errors.New("timed out")}
	}
	if err == nil {
		return errors.New("watch ended before the condition was met")
	}
	return err
}

// writeDocument writes objects as one JSON or YAML document: a single one
// bare, else {"items": [...]}.
func writeDocument(w interface{ Write([]byte) (int, error) }, format string, objs []proto.Message, single bool) error {
	var doc any
	if single && len(objs) == 1 {
		js, err := marshalJSON(objs[0])
		if err != nil {
			return err
		}
		doc = js
	} else {
		items := make([]json.RawMessage, 0, len(objs))
		for _, o := range objs {
			js, err := marshalJSON(o)
			if err != nil {
				return err
			}
			items = append(items, js)
		}
		doc = map[string]any{"items": items}
	}
	return writeJSONOrYAML(w, format, doc)
}
