package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"sigs.k8s.io/yaml"

	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/object"
)

var outputFormats = []string{"table", "json", "yaml", "name"}

func checkOutput(format string) error {
	for _, f := range outputFormats {
		if format == f {
			return nil
		}
	}
	return usageErr("unknown output %q; want one of %s", format, strings.Join(outputFormats, ", "))
}

func marshalJSON(m proto.Message) (json.RawMessage, error) {
	return protojson.Marshal(m)
}

// writeJSONOrYAML writes doc indented, as JSON or as YAML.
func writeJSONOrYAML(w io.Writer, format string, doc any) error {
	js, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if format == "yaml" {
		y, err := yaml.JSONToYAML(js)
		if err != nil {
			return err
		}
		_, err = w.Write(y)
		return err
	}
	_, err = fmt.Fprintln(w, string(js))
	return err
}

// printObjects writes objects in the format. A single object (from a get by
// name) prints bare; otherwise JSON and YAML wrap them in {"items": [...]} so
// the output is always one document.
func (a *app) printObjects(w io.Writer, format string, r registry.Resource, objs []proto.Message, single, allProjects bool) error {
	switch format {
	case "json", "yaml":
		return writeDocument(w, format, objs, single)
	case "name":
		for _, o := range objs {
			if _, err := fmt.Fprintln(w, objectName(r, o)); err != nil {
				return err
			}
		}
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	header := "NAME\tREADY\tPHASE\tAGE"
	if allProjects && r.ProjectScoped() {
		header = "PROJECT\t" + header
	}
	_, _ = fmt.Fprintln(tw, header)
	for _, o := range objs {
		meta := object.Meta(o)
		row := fmt.Sprintf("%s\t%s\t%s\t%s", meta.GetName(), readyColumn(o), phaseColumn(o), age(a.opts.Now(), meta))
		if allProjects && r.ProjectScoped() {
			row = meta.GetProject() + "\t" + row
		}
		_, _ = fmt.Fprintln(tw, row)
	}
	return tw.Flush()
}

func objectName(r registry.Resource, o proto.Message) string {
	return strings.ToLower(r.Kind) + "/" + object.Meta(o).GetName()
}

func readyColumn(o proto.Message) string {
	c := condition(o, "Ready")
	if c == nil {
		return "-"
	}
	switch c.GetStatus() {
	case metav1.ConditionStatus_CONDITION_STATUS_TRUE:
		return "True"
	case metav1.ConditionStatus_CONDITION_STATUS_FALSE:
		return "False"
	}
	return "Unknown"
}

// phaseColumn is the status's phase, such as READY for BOX_PHASE_READY;
// "-" for a kind without one.
func phaseColumn(o proto.Message) string {
	st := object.Status(o).ProtoReflect()
	fd := st.Descriptor().Fields().ByName("phase")
	if fd == nil || fd.Kind() != protoreflect.EnumKind {
		return "-"
	}
	v := fd.Enum().Values().ByNumber(st.Get(fd).Enum())
	if v == nil || v.Number() == 0 {
		return "-"
	}
	name := string(v.Name())
	// The enum's values share a prefix of its name, such as BOX_PHASE_.
	if i := strings.Index(name, "PHASE_"); i >= 0 {
		name = name[i+len("PHASE_"):]
	}
	return name
}

func age(now time.Time, meta *metav1.ObjectMeta) string {
	if meta.GetCreateTime() == nil {
		return "-"
	}
	d := now.Sub(meta.GetCreateTime().AsTime())
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// condition returns the status condition of the given type, if any.
func condition(o proto.Message, typ string) *metav1.Condition {
	st := object.Status(o).ProtoReflect()
	fd := st.Descriptor().Fields().ByName("conditions")
	if fd == nil {
		return nil
	}
	list := st.Get(fd).List()
	for i := range list.Len() {
		c, ok := list.Get(i).Message().Interface().(*metav1.Condition)
		if ok && c.GetType() == typ {
			return c
		}
	}
	return nil
}
