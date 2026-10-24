// Command protoc-gen-hangar generates the resource registry and the per-kind
// Connect handler shims from the resource declarations in proto/. See
// docs/decisions/resource-codegen.md.
//
// It runs with strategy "all" in one of three modes: mode=registry writes the
// public registry package under gen/go, mode=schema the JSON Schema of every
// kind beside it, and mode=server writes the handler shims under internal/.
package main

import (
	"flag"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/pluginpb"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
)

const (
	registryPath = protogen.GoImportPath("github.com/YukinaMochizuki/hangar/gen/go/hangar/registry")
	serverPath   = protogen.GoImportPath("github.com/YukinaMochizuki/hangar/internal/apiserver/servicegen")
	resourcePath = protogen.GoImportPath("github.com/YukinaMochizuki/hangar/internal/apimachinery/resource")
	metaPath     = protogen.GoImportPath("github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1")

	connectPath      = protogen.GoImportPath("connectrpc.com/connect")
	contextPath      = protogen.GoImportPath("context")
	httpPath         = protogen.GoImportPath("net/http")
	slicesPath       = protogen.GoImportPath("slices")
	stringsPath      = protogen.GoImportPath("strings")
	protoreflectPath = protogen.GoImportPath("google.golang.org/protobuf/reflect/protoreflect")
)

func main() {
	var flags flag.FlagSet
	mode := flags.String("mode", "", "registry, schema or server")
	domain := flags.String("domain", "", "suffix of every API group, such as hangarlab.cloud")
	protogen.Options{ParamFunc: flags.Set}.Run(func(gen *protogen.Plugin) error {
		gen.SupportedFeatures = uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL)
		kinds, err := collect(gen)
		if err != nil {
			return err
		}
		switch *mode {
		case "registry":
			if *domain == "" {
				return fmt.Errorf("mode=registry needs domain=<suffix>")
			}
			genRegistry(gen, kinds, *domain)
		case "schema":
			if *domain == "" {
				return fmt.Errorf("mode=schema needs domain=<suffix>")
			}
			return genSchema(gen, kinds, *domain)
		case "server":
			genServer(gen, kinds)
		default:
			return fmt.Errorf("unknown mode %q; want registry, schema or server", *mode)
		}
		return nil
	})
}

// kind is one declared resource and the parts of its service the shims use.
type kind struct {
	group, version string
	msg            *protogen.Message
	opts           *metav1.ResourceOptions
	connectPath    protogen.GoImportPath
	service        *protogen.Service
	get, list      *protogen.Method
	watch, apply   *protogen.Method
	del, status    *protogen.Method
	// objectField is the Go name of the field carrying the object in
	// single-object responses, such as RoleBinding.
	objectField string
}

func (k *kind) name() string        { return k.opts.GetKind() }
func (k *kind) projectScoped() bool { return k.opts.GetScope() == metav1.Scope_SCOPE_PROJECT }

func collect(gen *protogen.Plugin) ([]*kind, error) {
	var kinds []*kind
	for _, f := range gen.Files {
		if !f.Generate {
			continue
		}
		for _, m := range f.Messages {
			if !proto.HasExtension(m.Desc.Options(), metav1.E_Resource) {
				continue
			}
			k, err := newKind(f, m)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", m.Desc.FullName(), err)
			}
			kinds = append(kinds, k)
		}
	}
	sort.Slice(kinds, func(i, j int) bool {
		if kinds[i].group != kinds[j].group {
			return kinds[i].group < kinds[j].group
		}
		return kinds[i].name() < kinds[j].name()
	})
	return kinds, nil
}

func newKind(f *protogen.File, m *protogen.Message) (*kind, error) {
	parts := strings.Split(string(f.Desc.Package()), ".")
	if len(parts) != 3 || parts[0] != "hangar" {
		return nil, fmt.Errorf("package %s is not hangar.<group>.<version>", f.Desc.Package())
	}
	k := &kind{
		group:       parts[1],
		version:     parts[2],
		msg:         m,
		opts:        proto.GetExtension(m.Desc.Options(), metav1.E_Resource).(*metav1.ResourceOptions),
		connectPath: f.GoImportPath + "/" + protogen.GoImportPath(f.GoPackageName) + "connect",
	}
	for _, s := range f.Services {
		if s.GoName == k.name()+"Service" {
			k.service = s
		}
	}
	if k.service == nil {
		return nil, fmt.Errorf("no %sService in %s", k.name(), f.Desc.Path())
	}
	for _, mt := range k.service.Methods {
		n := mt.GoName
		switch {
		case n == "Get"+k.name():
			k.get = mt
		case strings.HasPrefix(n, "List"):
			k.list = mt
		case strings.HasPrefix(n, "Watch"):
			k.watch = mt
		case n == "Apply"+k.name():
			k.apply = mt
		case n == "Delete"+k.name():
			k.del = mt
		case n == "Update"+k.name()+"Status":
			k.status = mt
		}
	}
	for verb, mt := range map[string]*protogen.Method{
		"Get": k.get, "List": k.list, "Watch": k.watch,
		"Apply": k.apply, "Delete": k.del, "UpdateStatus": k.status,
	} {
		if mt == nil {
			return nil, fmt.Errorf("%sService has no %s method", k.name(), verb)
		}
	}
	if len(k.get.Output.Fields) == 0 {
		return nil, fmt.Errorf("%s has no fields", k.get.Output.Desc.FullName())
	}
	k.objectField = k.get.Output.Fields[0].GoName
	return k, nil
}

const header = "// Code generated by protoc-gen-hangar. DO NOT EDIT.\n"

func genRegistry(gen *protogen.Plugin, kinds []*kind, domain string) {
	g := gen.NewGeneratedFile("hangar/registry/registry.go", registryPath)
	g.P(header)
	g.P("// Package registry lists every API resource kind, generated from the")
	g.P("// resource declarations in proto/.")
	g.P("package registry")
	g.P()
	g.P("// Domain is the suffix of every API group.")
	g.P("const Domain = ", fmt.Sprintf("%q", domain))
	g.P()
	g.P("// Resource describes one API resource kind.")
	g.P("type Resource struct {")
	g.P("Group string")
	g.P("Version string")
	g.P("Kind string")
	g.P("// Plural is the lowercase plural used in permissions and the CLI.")
	g.P("Plural string")
	g.P("ShortNames []string")
	g.P("Scope ", g.QualifiedGoIdent(metaPath.Ident("Scope")))
	g.P("Message ", g.QualifiedGoIdent(protoreflectPath.Ident("MessageType")))
	g.P("// Service is the fully qualified name of the kind's Connect service.")
	g.P("Service string")
	g.P("}")
	g.P()
	g.P("// APIVersion returns the apiVersion users write for this kind.")
	g.P("func (r Resource) APIVersion() string { return r.Group + \".\" + Domain + \"/\" + r.Version }")
	g.P()
	g.P("// ProjectScoped reports whether objects of this kind belong to a project.")
	g.P("func (r Resource) ProjectScoped() bool { return r.Scope == ", g.QualifiedGoIdent(metaPath.Ident("Scope_SCOPE_PROJECT")), " }")
	g.P()
	g.P("var (")
	for _, k := range kinds {
		g.P(k.name(), " = Resource{")
		g.P("Group: ", fmt.Sprintf("%q", k.group), ",")
		g.P("Version: ", fmt.Sprintf("%q", k.version), ",")
		g.P("Kind: ", fmt.Sprintf("%q", k.name()), ",")
		g.P("Plural: ", fmt.Sprintf("%q", k.opts.GetPlural()), ",")
		g.P("ShortNames: ", fmt.Sprintf("%#v", append([]string{}, k.opts.GetShortNames()...)), ",")
		g.P("Scope: ", g.QualifiedGoIdent(metaPath.Ident("Scope_"+k.opts.GetScope().String())), ",")
		g.P("Message: (*", g.QualifiedGoIdent(k.msg.GoIdent), ")(nil).ProtoReflect().Type(),")
		g.P("Service: ", fmt.Sprintf("%q", k.service.Desc.FullName()), ",")
		g.P("}")
	}
	g.P(")")
	g.P()
	g.P("// All lists every resource kind, ordered by group and kind.")
	g.P("var All = []Resource{")
	for _, k := range kinds {
		g.P(k.name(), ",")
	}
	g.P("}")
	g.P()
	g.P("// Lookup returns the kind with the given apiVersion and kind.")
	g.P("func Lookup(apiVersion, kind string) (Resource, bool) {")
	g.P("for _, r := range All {")
	g.P("if r.Kind == kind && r.APIVersion() == apiVersion {")
	g.P("return r, true")
	g.P("}")
	g.P("}")
	g.P("return Resource{}, false")
	g.P("}")
	g.P()
	g.P("// Find returns the kind a CLI argument names: its kind in any case, its")
	g.P("// plural, or one of its short names.")
	g.P("func Find(name string) (Resource, bool) {")
	g.P("n := ", g.QualifiedGoIdent(stringsPath.Ident("ToLower")), "(name)")
	g.P("for _, r := range All {")
	g.P("if ", g.QualifiedGoIdent(stringsPath.Ident("ToLower")), "(r.Kind) == n || r.Plural == n || ",
		g.QualifiedGoIdent(slicesPath.Ident("Contains")), "(r.ShortNames, n) {")
	g.P("return r, true")
	g.P("}")
	g.P("}")
	g.P("return Resource{}, false")
	g.P("}")
}

func genServer(gen *protogen.Plugin, kinds []*kind) {
	g := gen.NewGeneratedFile("services.go", serverPath)
	q := func(path protogen.GoImportPath, name string) string { return g.QualifiedGoIdent(path.Ident(name)) }
	ctx := q(contextPath, "Context")
	backend := q(resourcePath, "Backend")

	g.P(header)
	g.P("// Package servicegen holds the Connect handlers for every resource kind.")
	g.P("// Each translates typed requests into calls on a resource.Backend.")
	g.P("package servicegen")
	g.P()
	g.P("// Backends returns the backend implementing a kind.")
	g.P("type Backends func(", q(registryPath, "Resource"), ") ", backend)
	g.P()
	g.P("// Mount registers a handler for every resource kind on mux.")
	g.P("func Mount(mux *", q(httpPath, "ServeMux"), ", backends Backends, opts ...", q(connectPath, "HandlerOption"), ") {")
	for _, k := range kinds {
		g.P("mux.Handle(", q(k.connectPath, "New"+k.service.GoName+"Handler"),
			"(&", lowerFirst(k.service.GoName), "{b: backends(", q(registryPath, k.name()), ")}, opts...))")
	}
	g.P("}")

	for _, k := range kinds {
		g.P()
		genService(g, k, ctx, backend)
	}
}

func genService(g *protogen.GeneratedFile, k *kind, ctx, backend string) {
	q := func(path protogen.GoImportPath, name string) string { return g.QualifiedGoIdent(path.Ident(name)) }
	recv := lowerFirst(k.service.GoName)
	obj := g.QualifiedGoIdent(k.msg.GoIdent)
	req := func(m *protogen.Method) string {
		return "*" + q(connectPath, "Request") + "[" + g.QualifiedGoIdent(m.Input.GoIdent) + "]"
	}
	resp := func(m *protogen.Method) string {
		return "*" + q(connectPath, "Response") + "[" + g.QualifiedGoIdent(m.Output.GoIdent) + "]"
	}
	project := ""
	if k.projectScoped() {
		project = "Project: req.Msg.GetProject(), "
	}
	key := q(resourcePath, "Key") + "{" + project + "Name: req.Msg.GetName()}"
	single := func(m *protogen.Method, call string) {
		g.P("func (s *", recv, ") ", m.GoName, "(ctx ", ctx, ", req ", req(m), ") (", resp(m), ", error) {")
		g.P("obj, err := ", call)
		g.P("if err != nil {")
		g.P("return nil, err")
		g.P("}")
		g.P("return ", q(connectPath, "NewResponse"), "(&", g.QualifiedGoIdent(m.Output.GoIdent), "{", k.objectField, ": obj.(*", obj, ")}), nil")
		g.P("}")
		g.P()
	}

	g.P("type ", recv, " struct {")
	g.P("b ", backend)
	g.P("}")
	g.P()

	single(k.get, "s.b.Get(ctx, "+key+")")

	g.P("func (s *", recv, ") ", k.list.GoName, "(ctx ", ctx, ", req ", req(k.list), ") (", resp(k.list), ", error) {")
	g.P("res, err := s.b.List(ctx, ", q(resourcePath, "ListOptions"), "{", project,
		"LabelSelector: req.Msg.GetLabelSelector(), PageSize: req.Msg.GetPageSize(), PageToken: req.Msg.GetPageToken()})")
	g.P("if err != nil {")
	g.P("return nil, err")
	g.P("}")
	g.P("out := &", g.QualifiedGoIdent(k.list.Output.GoIdent), "{NextPageToken: res.NextPageToken, Revision: res.Revision}")
	g.P("for _, item := range res.Items {")
	g.P("out.Items = append(out.Items, item.(*", obj, "))")
	g.P("}")
	g.P("return ", q(connectPath, "NewResponse"), "(out), nil")
	g.P("}")
	g.P()

	g.P("func (s *", recv, ") ", k.watch.GoName, "(ctx ", ctx, ", req ", req(k.watch),
		", stream *", q(connectPath, "ServerStream"), "[", g.QualifiedGoIdent(k.watch.Output.GoIdent), "]) error {")
	g.P("opts := ", q(resourcePath, "WatchOptions"), "{", project,
		"LabelSelector: req.Msg.GetLabelSelector(), SinceRevision: req.Msg.GetSinceRevision()}")
	g.P("return s.b.Watch(ctx, opts, func(ev ", q(resourcePath, "Event"), ") error {")
	g.P("out := &", g.QualifiedGoIdent(k.watch.Output.GoIdent), "{Type: ev.Type, Revision: ev.Revision}")
	g.P("if ev.Object != nil {")
	g.P("out.", k.objectField, " = ev.Object.(*", obj, ")")
	g.P("}")
	g.P("return stream.Send(out)")
	g.P("})")
	g.P("}")
	g.P()

	g.P("func (s *", recv, ") ", k.apply.GoName, "(ctx ", ctx, ", req ", req(k.apply), ") (", resp(k.apply), ", error) {")
	g.P("in := req.Msg.Get", k.objectField, "()")
	g.P("if in == nil {")
	g.P("return nil, ", q(connectPath, "NewError"), "(", q(connectPath, "CodeInvalidArgument"),
		", ", q(protogen.GoImportPath("errors"), "New"), "(", fmt.Sprintf("%q", snake(k.objectField)+" is required"), "))")
	g.P("}")
	g.P("apply := s.b.Apply")
	g.P("if req.Msg.GetCreateOnly() {")
	g.P("apply = s.b.Create")
	g.P("}")
	g.P("res, err := apply(ctx, in)")
	g.P("if err != nil {")
	g.P("return nil, err")
	g.P("}")
	g.P("return ", q(connectPath, "NewResponse"), "(&", g.QualifiedGoIdent(k.apply.Output.GoIdent), "{",
		k.objectField, ": res.Object.(*", obj, "), Warnings: res.Warnings, Created: res.Created}), nil")
	g.P("}")
	g.P()

	single(k.del, "s.b.Delete(ctx, "+key+", req.Msg.ExpectedResourceVersion)")
	single(k.status, "s.b.UpdateStatus(ctx, "+key+", req.Msg.GetStatus(), req.Msg.ExpectedResourceVersion)")
}

func lowerFirst(s string) string { return strings.ToLower(s[:1]) + s[1:] }

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
