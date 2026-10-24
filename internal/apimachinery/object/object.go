// Package object reads and writes the envelope of any resource message
// through protobuf reflection, so generic code needs no per-kind types. The
// envelope shape is guaranteed by internal/apimachinery/schemacheck.
package object

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
)

func field(m proto.Message, name protoreflect.Name) (protoreflect.Message, protoreflect.FieldDescriptor) {
	rm := m.ProtoReflect()
	return rm, rm.Descriptor().Fields().ByName(name)
}

// Meta returns the object's metadata for reading and writing, creating it if
// it is unset.
func Meta(m proto.Message) *metav1.ObjectMeta {
	rm, fd := field(m, "metadata")
	return rm.Mutable(fd).Message().Interface().(*metav1.ObjectMeta)
}

// SetTypeMeta sets api_version and kind.
func SetTypeMeta(m proto.Message, apiVersion, kind string) {
	rm, fd := field(m, "api_version")
	rm.Set(fd, protoreflect.ValueOfString(apiVersion))
	rm, fd = field(m, "kind")
	rm.Set(fd, protoreflect.ValueOfString(kind))
}

// TypeMeta returns api_version and kind.
func TypeMeta(m proto.Message) (apiVersion, kind string) {
	rm, fd := field(m, "api_version")
	apiVersion = rm.Get(fd).String()
	rm, fd = field(m, "kind")
	return apiVersion, rm.Get(fd).String()
}

// Spec returns the object's spec. An unset spec reads as an empty one.
func Spec(m proto.Message) proto.Message { return sub(m, "spec") }

// Status returns the object's status. An unset status reads as an empty one.
func Status(m proto.Message) proto.Message { return sub(m, "status") }

func sub(m proto.Message, name protoreflect.Name) proto.Message {
	rm, fd := field(m, name)
	if !rm.Has(fd) {
		return rm.NewField(fd).Message().Interface()
	}
	return rm.Get(fd).Message().Interface()
}

// SetStatus replaces the object's status; nil clears it.
func SetStatus(m proto.Message, status proto.Message) {
	rm, fd := field(m, "status")
	if status == nil {
		rm.Clear(fd)
		return
	}
	rm.Set(fd, protoreflect.ValueOfMessage(status.ProtoReflect()))
}

// CopyStatus makes dst's status what src's is, including unset.
func CopyStatus(dst, src proto.Message) {
	srm, fd := field(src, "status")
	if !srm.Has(fd) {
		SetStatus(dst, nil)
		return
	}
	SetStatus(dst, proto.Clone(srm.Get(fd).Message().Interface()))
}

// SpecEqual reports whether two objects of the same kind have equal specs,
// treating an unset spec as empty.
func SpecEqual(a, b proto.Message) bool { return proto.Equal(Spec(a), Spec(b)) }

// ImmutableChanges returns the names of spec fields marked immutable whose
// value differs between old and updated.
func ImmutableChanges(old, updated proto.Message) []string {
	return immutableChanges(Spec(old).ProtoReflect(), Spec(updated).ProtoReflect(), "spec")
}

func immutableChanges(old, updated protoreflect.Message, path string) []string {
	var out []string
	fields := old.Descriptor().Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		name := path + "." + string(fd.Name())
		opts := proto.GetExtension(fd.Options(), metav1.E_Field).(*metav1.FieldOptions)
		switch {
		case opts.GetImmutable():
			if !equalField(old, updated, fd) {
				out = append(out, name)
			}
		case fd.Kind() == protoreflect.MessageKind && !fd.IsList() && !fd.IsMap() && (old.Has(fd) || updated.Has(fd)):
			// An unset message reads as an empty one, so clearing a parent
			// still counts as changing the immutable fields inside it.
			out = append(out, immutableChanges(old.Get(fd).Message(), updated.Get(fd).Message(), name)...)
		}
	}
	return out
}

func equalField(a, b protoreflect.Message, fd protoreflect.FieldDescriptor) bool {
	if a.Has(fd) != b.Has(fd) {
		return false
	}
	return a.Get(fd).Equal(b.Get(fd))
}
