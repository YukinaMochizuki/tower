// Package schemacheck verifies that every resource declared in proto/ has the
// envelope and service shape that codegen and the generic machinery rely on.
// It runs as a test; see docs/decisions/resource-codegen.md.
package schemacheck

import (
	"fmt"
	"regexp"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
)

const (
	objectMeta = protoreflect.FullName("hangar.meta.v1.ObjectMeta")
	condition  = protoreflect.FullName("hangar.meta.v1.Condition")
	eventType  = protoreflect.FullName("hangar.meta.v1.EventType")
)

var (
	packagePattern = regexp.MustCompile(`^hangar\.[a-z]+\.v[0-9]+((alpha|beta)[0-9]+)?$`)
	lowerPattern   = regexp.MustCompile(`^[a-z]+$`)
)

// Result is what Check found: the resources it saw and every violation.
type Result struct {
	Kinds  []string
	Errors []error
}

// Check verifies every resource declared in files under the hangar package.
func Check(files *protoregistry.Files) Result {
	c := &checker{seen: map[string]string{}}
	files.RangeFiles(func(f protoreflect.FileDescriptor) bool {
		if strings.HasPrefix(string(f.Package()), "hangar.") {
			c.file(f)
		}
		return true
	})
	return c.res
}

type checker struct {
	res Result
	// seen maps every kind, plural and short name to the kind that claimed it,
	// because the CLI resolves all three through one namespace.
	seen map[string]string
}

func (c *checker) errorf(d protoreflect.Descriptor, format string, args ...any) {
	c.res.Errors = append(c.res.Errors, fmt.Errorf("%s: %s", d.FullName(), fmt.Sprintf(format, args...)))
}

func (c *checker) file(f protoreflect.FileDescriptor) {
	msgs := f.Messages()
	for i := range msgs.Len() {
		m := msgs.Get(i)
		if !proto.HasExtension(m.Options(), metav1.E_Resource) {
			continue
		}
		opts := proto.GetExtension(m.Options(), metav1.E_Resource).(*metav1.ResourceOptions)
		c.res.Kinds = append(c.res.Kinds, opts.GetKind())
		if !packagePattern.MatchString(string(f.Package())) {
			c.errorf(m, "package %s is not hangar.<group>.<version>", f.Package())
		}
		c.options(m, opts)
		c.envelope(m)
		c.service(f, m, opts)
	}
}

func (c *checker) claim(m protoreflect.MessageDescriptor, name string) {
	if prev, ok := c.seen[name]; ok {
		c.errorf(m, "name %q is already used by %s", name, prev)
		return
	}
	c.seen[name] = string(m.Name())
}

func (c *checker) options(m protoreflect.MessageDescriptor, opts *metav1.ResourceOptions) {
	if opts.GetKind() != string(m.Name()) {
		c.errorf(m, "resource kind %q must equal the message name", opts.GetKind())
	}
	if !lowerPattern.MatchString(opts.GetPlural()) {
		c.errorf(m, "resource plural %q must be lowercase letters", opts.GetPlural())
	}
	if opts.GetScope() == metav1.Scope_SCOPE_UNSPECIFIED {
		c.errorf(m, "resource scope is unspecified")
	}
	c.claim(m, strings.ToLower(opts.GetKind()))
	if opts.GetPlural() != strings.ToLower(opts.GetKind()) {
		c.claim(m, opts.GetPlural())
	}
	for _, s := range opts.GetShortNames() {
		if !lowerPattern.MatchString(s) {
			c.errorf(m, "short name %q must be lowercase letters", s)
		}
		c.claim(m, s)
	}
}

// field is the expected shape of one field. msg is the full name of the
// message or enum type, for message and enum fields.
type field struct {
	name     string
	kind     protoreflect.Kind
	msg      protoreflect.FullName
	repeated bool
	optional bool
}

func (c *checker) fields(m protoreflect.MessageDescriptor, want []field, numbered bool) {
	fs := m.Fields()
	if fs.Len() != len(want) {
		c.errorf(m, "has %d fields, want %d (%s)", fs.Len(), len(want), names(want))
	}
	for i, w := range want {
		f := fs.ByName(protoreflect.Name(w.name))
		if f == nil {
			c.errorf(m, "missing field %s", w.name)
			continue
		}
		if numbered && int(f.Number()) != i+1 {
			c.errorf(f, "is field %d, want %d", f.Number(), i+1)
		}
		if f.Kind() != w.kind {
			c.errorf(f, "is %s, want %s", f.Kind(), w.kind)
			continue
		}
		var typ protoreflect.FullName
		switch {
		case f.Message() != nil:
			typ = f.Message().FullName()
		case f.Enum() != nil:
			typ = f.Enum().FullName()
		}
		if w.msg != "" && typ != w.msg {
			c.errorf(f, "is %s, want %s", typ, w.msg)
		}
		if f.IsList() != w.repeated {
			c.errorf(f, "repeated is %t, want %t", f.IsList(), w.repeated)
		}
		if w.optional != f.HasOptionalKeyword() {
			c.errorf(f, "optional is %t, want %t", f.HasOptionalKeyword(), w.optional)
		}
	}
}

func names(fs []field) string {
	var out []string
	for _, f := range fs {
		out = append(out, f.name)
	}
	return strings.Join(out, ", ")
}

func (c *checker) envelope(m protoreflect.MessageDescriptor) {
	pkg := m.ParentFile().Package()
	spec := pkg.Append(protoreflect.Name(string(m.Name()) + "Spec"))
	status := pkg.Append(protoreflect.Name(string(m.Name()) + "Status"))
	c.fields(m, []field{
		{name: "api_version", kind: protoreflect.StringKind},
		{name: "kind", kind: protoreflect.StringKind},
		{name: "metadata", kind: protoreflect.MessageKind, msg: objectMeta},
		{name: "spec", kind: protoreflect.MessageKind, msg: spec},
		{name: "status", kind: protoreflect.MessageKind, msg: status},
	}, true)
	if sf := m.Fields().ByName("status"); sf != nil && sf.Message() != nil {
		st := sf.Message()
		want := []field{
			{name: "observed_generation", kind: protoreflect.Int64Kind},
			{name: "conditions", kind: protoreflect.MessageKind, msg: condition, repeated: true},
		}
		for i, w := range want {
			f := st.Fields().ByNumber(protoreflect.FieldNumber(i + 1))
			if f == nil || string(f.Name()) != w.name || f.Kind() != w.kind || f.IsList() != w.repeated ||
				(w.msg != "" && (f.Message() == nil || f.Message().FullName() != w.msg)) {
				c.errorf(st, "field %d must be %s", i+1, w.name)
			}
		}
	}
}

func (c *checker) service(f protoreflect.FileDescriptor, m protoreflect.MessageDescriptor, opts *metav1.ResourceOptions) {
	kind := string(m.Name())
	svc := f.Services().ByName(protoreflect.Name(kind + "Service"))
	if svc == nil {
		c.errorf(m, "no %sService in %s", kind, f.Path())
		return
	}
	obj := m.FullName()
	status := f.Package().Append(protoreflect.Name(kind + "Status"))
	objField := snake(kind)

	var key []field
	if opts.GetScope() == metav1.Scope_SCOPE_PROJECT {
		key = append(key, field{name: "project", kind: protoreflect.StringKind})
	}
	scope := key
	key = append(key, field{name: "name", kind: protoreflect.StringKind})
	expected := field{name: "expected_resource_version", kind: protoreflect.Int64Kind, optional: true}
	single := []field{{name: objField, kind: protoreflect.MessageKind, msg: obj}}

	type rpc struct {
		streaming bool
		req, resp []field
	}
	want := map[string]rpc{
		"Get" + kind: {req: key, resp: single},
		"List": {
			req: append(append([]field{}, scope...),
				field{name: "label_selector", kind: protoreflect.StringKind},
				field{name: "page_size", kind: protoreflect.Int32Kind},
				field{name: "page_token", kind: protoreflect.StringKind}),
			resp: []field{
				{name: "items", kind: protoreflect.MessageKind, msg: obj, repeated: true},
				{name: "next_page_token", kind: protoreflect.StringKind},
				{name: "revision", kind: protoreflect.Int64Kind},
			},
		},
		"Watch": {
			streaming: true,
			req: append(append([]field{}, scope...),
				field{name: "label_selector", kind: protoreflect.StringKind},
				field{name: "since_revision", kind: protoreflect.Int64Kind}),
			resp: []field{
				{name: "type", kind: protoreflect.EnumKind, msg: eventType},
				{name: objField, kind: protoreflect.MessageKind, msg: obj},
				{name: "revision", kind: protoreflect.Int64Kind},
			},
		},
		"Apply" + kind: {
			req: append(append([]field{}, single...), field{name: "create_only", kind: protoreflect.BoolKind}),
			resp: []field{
				{name: objField, kind: protoreflect.MessageKind, msg: obj},
				{name: "warnings", kind: protoreflect.StringKind, repeated: true},
				{name: "created", kind: protoreflect.BoolKind},
			},
		},
		"Delete" + kind: {req: append(append([]field{}, key...), expected), resp: single},
		"Update" + kind + "Status": {
			req: append(append([]field{}, key...),
				field{name: "status", kind: protoreflect.MessageKind, msg: status}, expected),
			resp: single,
		},
	}

	methods := svc.Methods()
	found := map[string]bool{}
	for i := range methods.Len() {
		md := methods.Get(i)
		name := string(md.Name())
		lookup := name
		for _, verb := range []string{"List", "Watch"} {
			if rest, ok := strings.CutPrefix(name, verb); ok && strings.EqualFold(rest, opts.GetPlural()) {
				lookup = verb
			}
		}
		w, ok := want[lookup]
		if !ok {
			c.errorf(md, "is not one of the standard resource methods")
			continue
		}
		found[lookup] = true
		if md.IsStreamingClient() || md.IsStreamingServer() != w.streaming {
			c.errorf(md, "server streaming must be %t and client streaming false", w.streaming)
		}
		c.fields(md.Input(), w.req, false)
		c.fields(md.Output(), w.resp, false)
	}
	for name := range want {
		if !found[name] {
			c.errorf(svc, "missing method %s", name)
		}
	}
}

func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}
