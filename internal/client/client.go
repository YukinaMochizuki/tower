// Package client calls the API of any resource kind without per-kind code: it
// finds the kind's service in the registry and speaks Connect with dynamic
// messages, converting objects to and from their generated types.
package client

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
)

// Client calls one API endpoint.
type Client struct {
	hc   connect.HTTPClient
	base string
	opts []connect.ClientOption
}

// New returns a client for the API at baseURL.
func New(hc connect.HTTPClient, baseURL string, opts ...connect.ClientOption) *Client {
	return &Client{hc: hc, base: strings.TrimSuffix(baseURL, "/"), opts: opts}
}

// UnixSocket returns an HTTP client that sends every request to the unix
// socket at path, as the node agent reaches the apiserver. Use it with
// UnixSocketURL.
func UnixSocket(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}
}

// UnixSocketURL is the base URL for a UnixSocket client; the host is never
// resolved.
const UnixSocketURL = "http://hangar.sock"

// Key names one object; Project is empty for global kinds.
type Key struct {
	Project string
	Name    string
}

// ListOptions selects objects; an empty Project lists every project the
// caller can read.
type ListOptions struct {
	Project       string
	LabelSelector string
	PageSize      int32
	PageToken     string
}

// ListResult is one page of objects.
type ListResult struct {
	Items         []proto.Message
	NextPageToken string
	Revision      int64
}

// Event is one watch event; Object is nil for bookmarks.
type Event struct {
	Type     metav1.EventType
	Object   proto.Message
	Revision int64
}

// ApplyResult is the stored object after an apply.
type ApplyResult struct {
	Object   proto.Message
	Created  bool
	Warnings []string
}

// method finds the RPC of r's service whose name starts with verb.
func method(r registry.Resource, verb string) (protoreflect.MethodDescriptor, error) {
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(r.Service))
	if err != nil {
		return nil, err
	}
	sd, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, fmt.Errorf("%s is not a service", r.Service)
	}
	methods := sd.Methods()
	for i := range methods.Len() {
		m := methods.Get(i)
		name := string(m.Name())
		if strings.HasPrefix(name, verb) && (verb != "Update" || strings.HasSuffix(name, "Status")) {
			return m, nil
		}
	}
	return nil, fmt.Errorf("%s has no %s method", r.Service, verb)
}

func (c *Client) client(md protoreflect.MethodDescriptor) *connect.Client[dynamicpb.Message, dynamicpb.Message] {
	opts := append([]connect.ClientOption{
		connect.WithSchema(md),
		connect.WithResponseInitializer(func(spec connect.Spec, msg any) error {
			m, ok := msg.(*dynamicpb.Message)
			if !ok {
				return fmt.Errorf("unexpected response type %T", msg)
			}
			*m = *dynamicpb.NewMessage(spec.Schema.(protoreflect.MethodDescriptor).Output())
			return nil
		}),
	}, c.opts...)
	url := c.base + "/" + string(md.Parent().FullName()) + "/" + string(md.Name())
	return connect.NewClient[dynamicpb.Message, dynamicpb.Message](c.hc, url, opts...)
}

// request builds a request message, setting the named fields that exist.
func request(md protoreflect.MethodDescriptor, fields map[string]any) (*dynamicpb.Message, error) {
	req := dynamicpb.NewMessage(md.Input())
	for name, v := range fields {
		fd := md.Input().Fields().ByName(protoreflect.Name(name))
		if fd == nil {
			continue
		}
		switch v := v.(type) {
		case string:
			if v != "" {
				req.Set(fd, protoreflect.ValueOfString(v))
			}
		case bool:
			req.Set(fd, protoreflect.ValueOfBool(v))
		case int32:
			req.Set(fd, protoreflect.ValueOfInt32(v))
		case int64:
			req.Set(fd, protoreflect.ValueOfInt64(v))
		case *int64:
			if v != nil {
				req.Set(fd, protoreflect.ValueOfInt64(*v))
			}
		case proto.Message:
			dm, err := toDynamic(v, fd.Message())
			if err != nil {
				return nil, err
			}
			req.Set(fd, protoreflect.ValueOfMessage(dm))
		default:
			return nil, fmt.Errorf("field %s: unsupported value %T", name, v)
		}
	}
	return req, nil
}

func toDynamic(m proto.Message, md protoreflect.MessageDescriptor) (*dynamicpb.Message, error) {
	b, err := proto.Marshal(m)
	if err != nil {
		return nil, err
	}
	dm := dynamicpb.NewMessage(md)
	return dm, proto.Unmarshal(b, dm)
}

// object converts a dynamic object into r's generated type.
func object(r registry.Resource, m protoreflect.Message) (proto.Message, error) {
	b, err := proto.Marshal(m.Interface())
	if err != nil {
		return nil, err
	}
	out := r.Message.New().Interface()
	return out, proto.Unmarshal(b, out)
}

// objectField returns the response field that carries the object: the only
// message field of r's kind.
func objectField(r registry.Resource, md protoreflect.MessageDescriptor) protoreflect.FieldDescriptor {
	fields := md.Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		if fd.Message() != nil && !fd.IsList() && string(fd.Message().Name()) == r.Kind {
			return fd
		}
	}
	return nil
}

func keyFields(r registry.Resource, k Key) map[string]any {
	f := map[string]any{"name": k.Name}
	if r.ProjectScoped() {
		f["project"] = k.Project
	}
	return f
}

func (c *Client) unary(ctx context.Context, r registry.Resource, verb string, fields map[string]any) (*dynamicpb.Message, protoreflect.MethodDescriptor, error) {
	md, err := method(r, verb)
	if err != nil {
		return nil, nil, err
	}
	req, err := request(md, fields)
	if err != nil {
		return nil, nil, err
	}
	res, err := c.client(md).CallUnary(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, nil, err
	}
	return res.Msg, md, nil
}

func (c *Client) single(ctx context.Context, r registry.Resource, verb string, fields map[string]any) (proto.Message, error) {
	res, md, err := c.unary(ctx, r, verb, fields)
	if err != nil {
		return nil, err
	}
	return object(r, res.Get(objectField(r, md.Output())).Message())
}

// Get returns one object.
func (c *Client) Get(ctx context.Context, r registry.Resource, k Key) (proto.Message, error) {
	return c.single(ctx, r, "Get", keyFields(r, k))
}

// Delete deletes one object and returns it as it was.
func (c *Client) Delete(ctx context.Context, r registry.Resource, k Key) (proto.Message, error) {
	return c.single(ctx, r, "Delete", keyFields(r, k))
}

// Apply creates or replaces an object.
func (c *Client) Apply(ctx context.Context, r registry.Resource, obj proto.Message) (ApplyResult, error) {
	return c.apply(ctx, r, obj, false)
}

// Create creates an object, and fails with AlreadyExists, changing nothing,
// when one of its name exists.
func (c *Client) Create(ctx context.Context, r registry.Resource, obj proto.Message) (ApplyResult, error) {
	return c.apply(ctx, r, obj, true)
}

func (c *Client) apply(ctx context.Context, r registry.Resource, obj proto.Message, createOnly bool) (ApplyResult, error) {
	md, err := method(r, "Apply")
	if err != nil {
		return ApplyResult{}, err
	}
	fd := objectField(r, md.Input())
	res, _, err := c.unary(ctx, r, "Apply", map[string]any{string(fd.Name()): obj, "create_only": createOnly})
	if err != nil {
		return ApplyResult{}, err
	}
	out, err := object(r, res.Get(objectField(r, md.Output())).Message())
	if err != nil {
		return ApplyResult{}, err
	}
	result := ApplyResult{Object: out}
	fields := md.Output().Fields()
	result.Created = res.Get(fields.ByName("created")).Bool()
	warnings := res.Get(fields.ByName("warnings")).List()
	for i := range warnings.Len() {
		result.Warnings = append(result.Warnings, warnings.Get(i).String())
	}
	return result, nil
}

// List returns one page of objects.
func (c *Client) List(ctx context.Context, r registry.Resource, opts ListOptions) (ListResult, error) {
	fields := map[string]any{
		"label_selector": opts.LabelSelector, "page_size": opts.PageSize, "page_token": opts.PageToken,
	}
	if r.ProjectScoped() {
		fields["project"] = opts.Project
	}
	res, md, err := c.unary(ctx, r, "List", fields)
	if err != nil {
		return ListResult{}, err
	}
	out := md.Output().Fields()
	items := res.Get(out.ByName("items")).List()
	result := ListResult{
		NextPageToken: res.Get(out.ByName("next_page_token")).String(),
		Revision:      res.Get(out.ByName("revision")).Int(),
	}
	for i := range items.Len() {
		obj, err := object(r, items.Get(i).Message())
		if err != nil {
			return ListResult{}, err
		}
		result.Items = append(result.Items, obj)
	}
	return result, nil
}

// ListAll follows page tokens and returns every matching object, with the
// revision of the first page: each page is read on its own, so only a watch
// from the earliest of them misses no change to an object already listed.
func (c *Client) ListAll(ctx context.Context, r registry.Resource, opts ListOptions) ([]proto.Message, int64, error) {
	var all []proto.Message
	var rev int64
	for first := true; ; first = false {
		page, err := c.List(ctx, r, opts)
		if err != nil {
			return nil, 0, err
		}
		if first {
			rev = page.Revision
		}
		all = append(all, page.Items...)
		if page.NextPageToken == "" {
			return all, rev, nil
		}
		opts.PageToken = page.NextPageToken
	}
}

// Watch streams events after since (0 means from now) until ctx ends, the
// stream ends, or fn returns an error.
func (c *Client) Watch(ctx context.Context, r registry.Resource, opts ListOptions, since int64, fn func(Event) error) error {
	md, err := method(r, "Watch")
	if err != nil {
		return err
	}
	fields := map[string]any{"label_selector": opts.LabelSelector, "since_revision": since}
	if r.ProjectScoped() {
		fields["project"] = opts.Project
	}
	req, err := request(md, fields)
	if err != nil {
		return err
	}
	stream, err := c.client(md).CallServerStream(ctx, connect.NewRequest(req))
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	out := md.Output().Fields()
	objField := objectField(r, md.Output())
	for stream.Receive() {
		msg := stream.Msg()
		ev := Event{
			Type:     metav1.EventType(msg.Get(out.ByName("type")).Enum()),
			Revision: msg.Get(out.ByName("revision")).Int(),
		}
		if msg.Has(objField) {
			if ev.Object, err = object(r, msg.Get(objField).Message()); err != nil {
				return err
			}
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
	return stream.Err()
}
