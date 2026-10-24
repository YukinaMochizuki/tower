package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const schemaPath = protogen.GoImportPath("github.com/YukinaMochizuki/hangar/gen/go/hangar/schema")

// genSchema writes one JSON Schema per kind, of its objects as protojson
// writes them, with the proto comments as descriptions, and the package that
// embeds them. See docs/decisions/api-schema.md.
func genSchema(gen *protogen.Plugin, kinds []*kind, domain string) error {
	messages := map[protoreflect.FullName]*protogen.Message{}
	for _, f := range gen.Files {
		var walk func([]*protogen.Message)
		walk = func(ms []*protogen.Message) {
			for _, m := range ms {
				messages[m.Desc.FullName()] = m
				walk(m.Messages)
			}
		}
		walk(f.Messages)
	}
	for _, k := range kinds {
		s := &schemaWriter{messages: messages, defs: map[string]any{}}
		root := s.object(k.msg)
		if s.err != nil {
			return fmt.Errorf("%s: %w", k.msg.Desc.FullName(), s.err)
		}
		props := root["properties"].(map[string]any)
		apiVersion := k.group + "." + domain + "/" + k.version
		props["apiVersion"].(map[string]any)["const"] = apiVersion
		props["kind"].(map[string]any)["const"] = k.name()
		props["status"].(map[string]any)["readOnly"] = true
		root["$schema"] = "https://json-schema.org/draft/2020-12/schema"
		root["title"] = k.name()
		root["required"] = []string{"apiVersion", "kind", "metadata"}
		root["$defs"] = s.defs
		var js bytes.Buffer
		enc := json.NewEncoder(&js)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(root); err != nil {
			return fmt.Errorf("%s: %w", k.msg.Desc.FullName(), err)
		}
		g := gen.NewGeneratedFile("hangar/schema/"+schemaFile(k.group, k.version, k.name()), schemaPath)
		_, _ = g.Write(js.Bytes())
	}

	g := gen.NewGeneratedFile("hangar/schema/schema.go", schemaPath)
	g.P(header)
	g.P("// Package schema holds the JSON Schema of every API resource kind,")
	g.P("// generated from proto/ with its comments as descriptions.")
	g.P("package schema")
	g.P()
	g.P("import _ \"embed\"")
	g.P()
	for _, k := range kinds {
		g.P("//go:embed ", schemaFile(k.group, k.version, k.name()))
		g.P("var ", lowerFirst(k.group), k.version, k.name(), " []byte")
		g.P()
	}
	g.P("// Get returns the JSON Schema of a kind, by its group, version and kind.")
	g.P("func Get(group, version, kind string) ([]byte, bool) {")
	g.P("switch group + \"/\" + version + \"/\" + kind {")
	for _, k := range kinds {
		g.P("case ", fmt.Sprintf("%q", k.group+"/"+k.version+"/"+k.name()), ":")
		g.P("return ", lowerFirst(k.group), k.version, k.name(), ", true")
	}
	g.P("}")
	g.P("return nil, false")
	g.P("}")
	return nil
}

func schemaFile(group, version, kind string) string {
	return group + "_" + version + "_" + strings.ToLower(kind) + ".json"
}

// schemaWriter builds one kind's schema; every message it reaches is a
// definition under $defs, by its full name.
type schemaWriter struct {
	messages map[protoreflect.FullName]*protogen.Message
	defs     map[string]any
	// err is the first field found whose JSON form the writer cannot say.
	err error
}

func (s *schemaWriter) object(m *protogen.Message) map[string]any {
	props := map[string]any{}
	for _, f := range m.Fields {
		p := s.field(f)
		if d := fieldDescription(f); d != "" {
			p["description"] = d
		}
		props[f.Desc.JSONName()] = p
	}
	o := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if d := description(m.Comments.Leading); d != "" {
		o["description"] = d
	}
	return o
}

func (s *schemaWriter) field(f *protogen.Field) map[string]any {
	switch {
	case f.Desc.IsMap():
		return map[string]any{"type": "object", "additionalProperties": s.value(f.Desc.MapValue())}
	case f.Desc.IsList():
		return map[string]any{"type": "array", "items": s.value(f.Desc)}
	}
	return s.value(f.Desc)
}

// value is the schema of one value of fd: the field itself, or an item or a
// map value of it.
func (s *schemaWriter) value(fd protoreflect.FieldDescriptor) map[string]any {
	switch fd.Kind() {
	case protoreflect.StringKind:
		return map[string]any{"type": "string"}
	case protoreflect.BytesKind:
		return map[string]any{"type": "string", "contentEncoding": "base64"}
	case protoreflect.BoolKind:
		return map[string]any{"type": "boolean"}
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return map[string]any{"type": "integer", "format": "int32"}
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return map[string]any{"type": "integer", "format": "uint32", "minimum": 0}
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		// protojson writes 64-bit integers as strings of digits and reads
		// either; the pattern binds only the string.
		return map[string]any{"type": []string{"integer", "string"}, "format": "int64", "pattern": `^-?[0-9]+$`}
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return map[string]any{"type": []string{"integer", "string"}, "format": "uint64", "pattern": `^[0-9]+$`}
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return map[string]any{"type": "number"}
	case protoreflect.EnumKind:
		var values []string
		for i := range fd.Enum().Values().Len() {
			if v := fd.Enum().Values().Get(i); v.Number() != 0 {
				values = append(values, string(v.Name()))
			}
		}
		return map[string]any{"type": "string", "enum": values}
	}
	md := fd.Message()
	switch md.FullName() {
	case "google.protobuf.Timestamp":
		return map[string]any{"type": "string", "format": "date-time"}
	case "google.protobuf.Duration":
		return map[string]any{"type": "string", "pattern": `^-?[0-9]+(\.[0-9]{1,9})?s$`, "examples": []string{"30s", "1.5s"}}
	case "google.protobuf.Empty":
		return map[string]any{"type": "object", "maxProperties": 0}
	}
	if md.ParentFile().Package() == "google.protobuf" {
		// protojson gives the rest of them, such as Struct or the wrappers,
		// JSON forms of their own, which this writer does not know.
		if s.err == nil {
			s.err = fmt.Errorf("field %s is a %s, which has no JSON Schema here; add one to schemaWriter.value", fd.FullName(), md.FullName())
		}
		return map[string]any{}
	}
	name := string(md.FullName())
	if _, done := s.defs[name]; !done {
		m, ok := s.messages[md.FullName()]
		if !ok {
			// A message of a file not given to the plugin: its shape is all
			// the descriptor says.
			s.defs[name] = map[string]any{"type": "object"}
		} else {
			// Claimed before it is built, so a message that holds itself
			// refers to its definition rather than building it forever.
			s.defs[name] = nil
			s.defs[name] = s.object(m)
		}
	}
	return map[string]any{"$ref": "#/$defs/" + name}
}

// fieldDescription is a field's comments, what its enum's values mean, and
// which fields it excludes when it is one of a oneof.
func fieldDescription(f *protogen.Field) string {
	d := description(f.Comments.Leading)
	if t := description(f.Comments.Trailing); t != "" {
		d = strings.TrimSpace(d + "\n\n" + t)
	}
	if e := f.Enum; e != nil {
		if d == "" {
			d = description(e.Comments.Leading)
		}
		var values []string
		for _, v := range e.Values {
			if vd := description(v.Comments.Leading); vd != "" && v.Desc.Number() != 0 {
				values = append(values, "- "+string(v.Desc.Name())+": "+vd)
			}
		}
		if len(values) > 0 {
			d = strings.TrimSpace(d + "\n\n" + strings.Join(values, "\n"))
		}
	}
	if o := f.Oneof; o != nil && !o.Desc.IsSynthetic() {
		var names []string
		for _, of := range o.Fields {
			names = append(names, of.Desc.JSONName())
		}
		d = strings.TrimSpace(d + "\n\nSet at most one of " + strings.Join(names, ", ") + ".")
	}
	return d
}

// description turns a proto comment into prose: lines of a paragraph are
// joined, and paragraphs, list items and indented lines keep their own lines.
func description(c protogen.Comments) string {
	var b strings.Builder
	newLine := true
	for _, line := range strings.Split(strings.TrimRight(string(c), "\n"), "\n") {
		line = strings.TrimPrefix(line, " ")
		switch {
		case strings.TrimSpace(line) == "":
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			newLine = true
			continue
		case !newLine && (strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") || strings.HasPrefix(line, "  ")):
			b.WriteString("\n")
		case !newLine:
			b.WriteString(" ")
		}
		b.WriteString(line)
		newLine = false
	}
	return strings.TrimSpace(b.String())
}
