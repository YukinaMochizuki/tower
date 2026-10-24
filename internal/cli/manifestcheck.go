package cli

import (
	"fmt"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// fieldError is a manifest's mistake at one field: where it is, and what
// would be right there.
type fieldError struct {
	line int
	path string
	msg  string
}

func fieldErr(line int, path, msg string) *fieldError {
	return &fieldError{line: line, path: path, msg: msg}
}

func (e *fieldError) Error() string {
	return fmt.Sprintf("line %d: %s: %s", e.line, e.path, e.msg)
}

// checkFields checks the object n of kind against md before protojson
// decodes it, which would say neither where in the file a mistake is nor what
// the field takes. It finds unknown fields, enum values that are none of the
// enum's, and values of the wrong shape; protojson stays the last word.
func checkFields(n *yaml.Node, md protoreflect.MessageDescriptor, _ string) error {
	return checkMessage(n, md, "")
}

func checkMessage(n *yaml.Node, md protoreflect.MessageDescriptor, path string) error {
	n = resolve(n)
	if isNull(n) {
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fieldErr(n.Line, orTop(path), "want an object, with fields "+fieldNames(md))
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Value == "<<" {
			if err := checkMerge(v, md, path); err != nil {
				return err
			}
			continue
		}
		at := join(path, k.Value)
		fd := md.Fields().ByJSONName(k.Value)
		if fd == nil {
			fd = md.Fields().ByName(protoreflect.Name(k.Value))
		}
		if fd == nil {
			return fieldErr(k.Line, at, unknownField(k.Value, md, path))
		}
		if err := checkField(v, fd, at); err != nil {
			return err
		}
	}
	return nil
}

// checkMerge checks what a merge key brings in, one mapping or a list of them,
// as fields of md.
func checkMerge(v *yaml.Node, md protoreflect.MessageDescriptor, path string) error {
	v = resolve(v)
	if v.Kind != yaml.SequenceNode {
		return checkMessage(v, md, path)
	}
	for _, m := range v.Content {
		if err := checkMessage(m, md, path); err != nil {
			return err
		}
	}
	return nil
}

func checkField(v *yaml.Node, fd protoreflect.FieldDescriptor, at string) error {
	v = resolve(v)
	if isNull(v) {
		return nil
	}
	switch {
	case fd.IsMap():
		if v.Kind != yaml.MappingNode {
			return fieldErr(v.Line, at, "want an object of keys and values")
		}
		for j := 0; j+1 < len(v.Content); j += 2 {
			if err := checkValue(v.Content[j+1], fd.MapValue(), fmt.Sprintf("%s[%s]", at, v.Content[j].Value)); err != nil {
				return err
			}
		}
		return nil
	case fd.IsList():
		if v.Kind != yaml.SequenceNode {
			return fieldErr(v.Line, at, "want a list")
		}
		for j, item := range v.Content {
			if err := checkValue(item, fd, fmt.Sprintf("%s[%d]", at, j)); err != nil {
				return err
			}
		}
		return nil
	}
	return checkValue(v, fd, at)
}

// checkValue checks one value of fd: the field itself, or an item or map
// value of it.
func checkValue(v *yaml.Node, fd protoreflect.FieldDescriptor, at string) error {
	v = resolve(v)
	if isNull(v) {
		return nil
	}
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		md := fd.Message()
		if wellKnown(md) {
			// Their JSON forms are strings or numbers protojson checks well.
			return nil
		}
		return checkMessage(v, md, at)
	}
	if v.Kind != yaml.ScalarNode {
		return fieldErr(v.Line, at, "want a single value, not a list or an object")
	}
	switch fd.Kind() {
	case protoreflect.EnumKind:
		return checkEnum(v, fd.Enum(), at)
	case protoreflect.BoolKind:
		if v.ShortTag() != "!!bool" {
			return fieldErr(v.Line, at, fmt.Sprintf("%q is not true or false", v.Value))
		}
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		if _, err := strconv.ParseInt(v.Value, 10, 64); err != nil {
			return fieldErr(v.Line, at, fmt.Sprintf("%q is not a whole number", v.Value))
		}
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind, protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		if _, err := strconv.ParseUint(v.Value, 10, 64); err != nil {
			return fieldErr(v.Line, at, fmt.Sprintf("%q is not a whole number of 0 or more", v.Value))
		}
	}
	return nil
}

func checkEnum(v *yaml.Node, ed protoreflect.EnumDescriptor, at string) error {
	if ed.Values().ByName(protoreflect.Name(v.Value)) != nil {
		return nil
	}
	if _, err := strconv.ParseInt(v.Value, 10, 32); err == nil {
		return nil
	}
	values := enumValues(ed)
	msg := fmt.Sprintf("%q is not a %s; want one of %s", v.Value, ed.Name(), strings.Join(values, ", "))
	// The last word of a value, such as running for
	// CONTAINER_RUN_STATE_RUNNING, is a likelier mistake than a typo.
	want := strings.ToUpper(v.Value)
	for _, name := range values {
		if strings.HasSuffix(name, "_"+want) {
			msg += fmt.Sprintf(" (did you mean %s?)", name)
			break
		}
	}
	return fieldErr(v.Line, at, msg)
}

// enumValues lists the values a manifest may write for ed, without the zero
// value an unset field already has.
func enumValues(ed protoreflect.EnumDescriptor) []string {
	var out []string
	for i := range ed.Values().Len() {
		if ev := ed.Values().Get(i); ev.Number() != 0 {
			out = append(out, string(ev.Name()))
		}
	}
	return out
}

func unknownField(name string, md protoreflect.MessageDescriptor, path string) string {
	var names []string
	for i := range md.Fields().Len() {
		names = append(names, md.Fields().Get(i).JSONName())
	}
	msg := "unknown field"
	if n, ok := closest(name, names); ok {
		msg += fmt.Sprintf("; did you mean %s?", n)
	}
	return fmt.Sprintf("%s; %s has %s", msg, orTop(path), fieldNames(md))
}

func fieldNames(md protoreflect.MessageDescriptor) string {
	var names []string
	for i := range md.Fields().Len() {
		names = append(names, md.Fields().Get(i).JSONName())
	}
	return strings.Join(names, ", ")
}

// wellKnown reports whether md is one of protobuf's own types, such as
// Timestamp, which JSON writes as a scalar.
func wellKnown(md protoreflect.MessageDescriptor) bool {
	return md.ParentFile().Package() == "google.protobuf"
}

func resolve(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null"
}

func join(path, field string) string {
	if path == "" {
		return field
	}
	return path + "." + field
}

func orTop(path string) string {
	if path == "" {
		return "the object"
	}
	return path
}
