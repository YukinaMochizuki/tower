package schemacheck_test

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/schemacheck"
)

func TestDeclaredResources(t *testing.T) {
	res := schemacheck.Check(protoregistry.GlobalFiles)
	for _, err := range res.Errors {
		t.Error(err)
	}
	var want []string
	for _, r := range registry.All {
		want = append(want, r.Kind)
	}
	slices.Sort(want)
	got := slices.Sorted(slices.Values(res.Kinds))
	if !slices.Equal(got, want) {
		t.Errorf("checked kinds %v, registry has %v", got, want)
	}
}

func TestViolations(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(container *descriptorpb.FileDescriptorProto)
		want   string
	}{
		{
			name: "envelope field renamed",
			mutate: func(f *descriptorpb.FileDescriptorProto) {
				message(f, "Container").Field[3].Name = proto.String("desired")
			},
			want: "missing field spec",
		},
		{
			name: "status without conditions",
			mutate: func(f *descriptorpb.FileDescriptorProto) {
				m := message(f, "ContainerStatus")
				m.Field = m.Field[:1]
			},
			want: "field 2 must be conditions",
		},
		{
			name: "watch method missing",
			mutate: func(f *descriptorpb.FileDescriptorProto) {
				s := f.Service[0]
				s.Method = slices.DeleteFunc(s.Method, func(m *descriptorpb.MethodDescriptorProto) bool {
					return m.GetName() == "WatchContainers"
				})
			},
			want: "missing method Watch",
		},
		{
			name: "watch not streaming",
			mutate: func(f *descriptorpb.FileDescriptorProto) {
				for _, m := range f.Service[0].Method {
					if m.GetName() == "WatchContainers" {
						m.ServerStreaming = proto.Bool(false)
					}
				}
			},
			want: "server streaming must be true",
		},
		{
			name: "project-scoped request without project",
			mutate: func(f *descriptorpb.FileDescriptorProto) {
				message(f, "GetContainerRequest").Field[0].Name = proto.String("tenant")
			},
			want: "missing field project",
		},
		{
			name: "expected_resource_version without presence",
			mutate: func(f *descriptorpb.FileDescriptorProto) {
				m := message(f, "DeleteContainerRequest")
				m.Field[2].Proto3Optional = nil
				m.Field[2].OneofIndex = nil
				m.OneofDecl = nil
			},
			want: "optional is false, want true",
		},
		{
			name: "plural not lowercase",
			mutate: func(f *descriptorpb.FileDescriptorProto) {
				setOptions(message(f, "Container"), func(o *metav1.ResourceOptions) { o.Plural = "Containers" })
			},
			want: "must be lowercase letters",
		},
		{
			name: "short name taken by another kind",
			mutate: func(f *descriptorpb.FileDescriptorProto) {
				setOptions(message(f, "Container"), func(o *metav1.ResourceOptions) { o.ShortNames = []string{"proj"} })
			},
			want: `name "proj" is already used by`,
		},
		{
			name: "scope unspecified",
			mutate: func(f *descriptorpb.FileDescriptorProto) {
				setOptions(message(f, "Container"), func(o *metav1.ResourceOptions) { o.Scope = metav1.Scope_SCOPE_UNSPECIFIED })
			},
			want: "resource scope is unspecified",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			container := protodesc.ToFileDescriptorProto((&computev1alpha1.Container{}).ProtoReflect().Descriptor().ParentFile())
			tt.mutate(container)
			files := build(t, container, (&tenancyv1alpha1.Project{}).ProtoReflect().Descriptor().ParentFile())
			res := schemacheck.Check(files)
			var msgs []string
			for _, err := range res.Errors {
				msgs = append(msgs, err.Error())
			}
			if !slices.ContainsFunc(msgs, func(m string) bool { return strings.Contains(m, tt.want) }) {
				t.Errorf("want an error containing %q, got %q", tt.want, msgs)
			}
		})
	}
}

func message(f *descriptorpb.FileDescriptorProto, name string) *descriptorpb.DescriptorProto {
	for _, m := range f.MessageType {
		if m.GetName() == name {
			return m
		}
	}
	panic("no message " + name)
}

func setOptions(m *descriptorpb.DescriptorProto, edit func(*metav1.ResourceOptions)) {
	o := proto.Clone(proto.GetExtension(m.Options, metav1.E_Resource).(*metav1.ResourceOptions)).(*metav1.ResourceOptions)
	edit(o)
	m.Options = proto.Clone(m.Options).(*descriptorpb.MessageOptions)
	proto.SetExtension(m.Options, metav1.E_Resource, o)
}

// build returns a registry holding the given files and everything they import.
func build(t *testing.T, primary *descriptorpb.FileDescriptorProto, others ...protoreflect.FileDescriptor) *protoregistry.Files {
	t.Helper()
	set := &descriptorpb.FileDescriptorSet{}
	added := map[string]bool{primary.GetName(): true}
	var add func(protoreflect.FileDescriptor)
	add = func(fd protoreflect.FileDescriptor) {
		if added[fd.Path()] {
			return
		}
		added[fd.Path()] = true
		imports := fd.Imports()
		for i := range imports.Len() {
			add(imports.Get(i).FileDescriptor)
		}
		set.File = append(set.File, protodesc.ToFileDescriptorProto(fd))
	}
	for _, dep := range primary.GetDependency() {
		fd, err := protoregistry.GlobalFiles.FindFileByPath(dep)
		if err != nil {
			t.Fatal(err)
		}
		add(fd)
	}
	for _, fd := range others {
		add(fd)
	}
	set.File = append(set.File, primary)
	files, err := protodesc.NewFiles(set)
	if err != nil {
		t.Fatal(err)
	}
	return files
}
