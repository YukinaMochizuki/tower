package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
)

// manifest is one object read from a file.
type manifest struct {
	source string
	r      registry.Resource
	obj    proto.Message
}

// readManifests reads every object in the given files, directories (their
// .yaml, .yml and .json files, in name order) or "-" for stdin.
func readManifests(paths []string, stdin io.Reader) ([]manifest, error) {
	var out []manifest
	for _, p := range paths {
		if p == "-" {
			ms, err := decodeManifests("stdin", stdin)
			if err != nil {
				return nil, err
			}
			out = append(out, ms...)
			continue
		}
		files, err := manifestFiles(p)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				return nil, err
			}
			ms, err := decodeManifests(f, bytes.NewReader(b))
			if err != nil {
				return nil, err
			}
			out = append(out, ms...)
		}
	}
	return out, nil
}

func manifestFiles(p string) ([]string, error) {
	info, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{p}, nil
	}
	var files []string
	err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".yaml", ".yml", ".json":
			if !d.IsDir() {
				files = append(files, path)
			}
		}
		return nil
	})
	slices.Sort(files)
	return files, err
}

// decodeManifests reads a stream of YAML (or JSON) documents. Each becomes the
// message its apiVersion and kind name, decoded strictly: an unknown field is
// an error, not something silently dropped. A number written where the
// message has a string, such as cpu: 2, is taken as its text.
func decodeManifests(source string, r io.Reader) ([]manifest, error) {
	dec := yaml.NewDecoder(r)
	var out []manifest
	for i := 1; ; i++ {
		var node yaml.Node
		err := dec.Decode(&node)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		where := fmt.Sprintf("%s, document %d", source, i)
		if err != nil {
			return nil, usageErr("%s: %v", where, err)
		}
		var doc any
		if err := node.Decode(&doc); err != nil {
			return nil, usageErr("%s: %v", where, err)
		}
		if doc == nil {
			continue
		}
		fields, ok := doc.(map[string]any)
		if !ok {
			return nil, usageErr("%s: not an object", where)
		}
		apiVersion, _ := fields["apiVersion"].(string)
		kind, _ := fields["kind"].(string)
		res, ok := registry.Lookup(apiVersion, kind)
		if !ok {
			return nil, fmt.Errorf("%s: %w", where, unknownManifestKind(apiVersion, kind))
		}
		if err := checkFields(node.Content[0], res.Message.Descriptor(), res.Kind); err != nil {
			return nil, usageErr("%s, %v", where, err)
		}
		numbersAsText(node.Content[0], res.Message.Descriptor())
		var typed any
		if err := node.Decode(&typed); err != nil {
			return nil, usageErr("%s: %v", where, err)
		}
		js, err := json.Marshal(typed)
		if err != nil {
			return nil, usageErr("%s: %v", where, err)
		}
		obj := res.Message.New().Interface()
		if err := protojson.Unmarshal(js, obj); err != nil {
			return nil, usageErr("%s: %s", where, protojsonPosition.ReplaceAllString(err.Error(), ""))
		}
		out = append(out, manifest{source: where, r: res, obj: obj})
	}
}

// protojsonPosition is where protojson says an error is: in the JSON made from
// the YAML, which the user never saw.
var protojsonPosition = regexp.MustCompile(`^proto: (\(line \d+:\d+\): )?`)

// numbersAsText retags every number in n that stands where md has a string
// field as a string, so it decodes as the text written: protojson takes no
// number for a string, and 1.10 or 1e3 must not come back as 1.1 or 1000.
func numbersAsText(n *yaml.Node, md protoreflect.MessageDescriptor) {
	if n.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i].Value, n.Content[i+1]
		fd := md.Fields().ByJSONName(key)
		if fd == nil {
			fd = md.Fields().ByName(protoreflect.Name(key))
		}
		switch {
		case fd == nil:
		case fd.IsMap():
			if val.Kind == yaml.MappingNode {
				for j := 1; j < len(val.Content); j += 2 {
					valueAsText(val.Content[j], fd.MapValue())
				}
			}
		case fd.IsList():
			if val.Kind == yaml.SequenceNode {
				for _, item := range val.Content {
					valueAsText(item, fd)
				}
			}
		default:
			valueAsText(val, fd)
		}
	}
}

// valueAsText applies numbersAsText to one value of field fd.
func valueAsText(n *yaml.Node, fd protoreflect.FieldDescriptor) {
	switch fd.Kind() {
	case protoreflect.StringKind:
		if t := n.ShortTag(); n.Kind == yaml.ScalarNode && (t == "!!int" || t == "!!float") {
			n.Tag = "!!str"
		}
	case protoreflect.MessageKind:
		numbersAsText(n, fd.Message())
	}
}
