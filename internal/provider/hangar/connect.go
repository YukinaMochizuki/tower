package hangar

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	hangarcompute "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1/computev1alpha1connect"
	hangarnetwork "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1/networkv1alpha1connect"
	hangarsecrets "github.com/YukinaMochizuki/hangar/gen/go/hangar/secrets/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/secrets/v1alpha1/secretsv1alpha1connect"
	hangarstorage "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1/storagev1alpha1connect"
	hangartenancy "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1/tenancyv1alpha1connect"
)

// Connect is API over hangar's own: its Connect services and its
// machines' WebSockets, at Endpoint, as the project's service token.
type Connect struct {
	// endpoint is the API's URL, such as https://api.hangar.example;
	// project the hangar project every object is in; token the project's
	// service token, sent as a bearer token; hc the client every call goes
	// through.
	endpoint, project, token string
	hc                       *http.Client

	networks   Kind[*hangarnetwork.Network]
	disks      Kind[*hangarstorage.Disk]
	containers Kind[*hangarcompute.Container]
	vms        Kind[*hangarcompute.VirtualMachine]
	secrets    Kind[*hangarsecrets.Secret]
	images     Kind[*hangarstorage.CustomImage]
	epsvcs     Kind[*hangarnetwork.EndpointService]
	neps       Kind[*hangarnetwork.NetworkEndpoint]
	sgs        Kind[*hangarnetwork.SecurityGroup]
	machines   computev1alpha1connect.MachineServiceClient
	globals    storagev1alpha1connect.ImageServiceClient
	projects   tenancyv1alpha1connect.ProjectServiceClient
}

var _ API = (*Connect)(nil)

// NewConnect returns the API of a project at endpoint, as token, through
// hc, the endpoint's Front's client, which every Connect of the endpoint
// shares; nil is the client of a Front of its own.
func NewConnect(endpoint, project, token string, hc *http.Client) *Connect {
	if hc == nil {
		hc = NewFront(FrontOptions{}).Client()
	}
	c := &Connect{endpoint: strings.TrimSuffix(endpoint, "/"), project: project, token: token, hc: hc}
	opts := []connect.ClientOption{connect.WithInterceptors(bearer(token))}
	nets := networkv1alpha1connect.NewNetworkServiceClient(hc, c.endpoint, opts...)
	c.networks = kind[*hangarnetwork.Network]{
		project: project, what: "network",
		get: func(ctx context.Context, name string) (*hangarnetwork.Network, error) {
			r, err := nets.GetNetwork(ctx, connect.NewRequest(&hangarnetwork.GetNetworkRequest{Project: project, Name: name}))
			return msg(r).GetNetwork(), err
		},
		list: func(ctx context.Context, sel, page string) ([]*hangarnetwork.Network, string, error) {
			r, err := nets.ListNetworks(ctx, connect.NewRequest(&hangarnetwork.ListNetworksRequest{Project: project, LabelSelector: sel, PageToken: page}))
			return msg(r).GetItems(), msg(r).GetNextPageToken(), err
		},
		apply: func(ctx context.Context, o *hangarnetwork.Network) (*hangarnetwork.Network, error) {
			o.GetMetadata().Project = project
			r, err := nets.ApplyNetwork(ctx, connect.NewRequest(&hangarnetwork.ApplyNetworkRequest{Network: o}))
			return msg(r).GetNetwork(), err
		},
		del: func(ctx context.Context, name string) error {
			_, err := nets.DeleteNetwork(ctx, connect.NewRequest(&hangarnetwork.DeleteNetworkRequest{Project: project, Name: name}))
			return err
		},
	}
	disks := storagev1alpha1connect.NewDiskServiceClient(hc, c.endpoint, opts...)
	c.disks = kind[*hangarstorage.Disk]{
		project: project, what: "disk",
		get: func(ctx context.Context, name string) (*hangarstorage.Disk, error) {
			r, err := disks.GetDisk(ctx, connect.NewRequest(&hangarstorage.GetDiskRequest{Project: project, Name: name}))
			return msg(r).GetDisk(), err
		},
		list: func(ctx context.Context, sel, page string) ([]*hangarstorage.Disk, string, error) {
			r, err := disks.ListDisks(ctx, connect.NewRequest(&hangarstorage.ListDisksRequest{Project: project, LabelSelector: sel, PageToken: page}))
			return msg(r).GetItems(), msg(r).GetNextPageToken(), err
		},
		apply: func(ctx context.Context, o *hangarstorage.Disk) (*hangarstorage.Disk, error) {
			o.GetMetadata().Project = project
			r, err := disks.ApplyDisk(ctx, connect.NewRequest(&hangarstorage.ApplyDiskRequest{Disk: o}))
			return msg(r).GetDisk(), err
		},
		del: func(ctx context.Context, name string) error {
			_, err := disks.DeleteDisk(ctx, connect.NewRequest(&hangarstorage.DeleteDiskRequest{Project: project, Name: name}))
			return err
		},
	}
	cts := computev1alpha1connect.NewContainerServiceClient(hc, c.endpoint, opts...)
	c.containers = kind[*hangarcompute.Container]{
		project: project, what: "container",
		get: func(ctx context.Context, name string) (*hangarcompute.Container, error) {
			r, err := cts.GetContainer(ctx, connect.NewRequest(&hangarcompute.GetContainerRequest{Project: project, Name: name}))
			return msg(r).GetContainer(), err
		},
		list: func(ctx context.Context, sel, page string) ([]*hangarcompute.Container, string, error) {
			r, err := cts.ListContainers(ctx, connect.NewRequest(&hangarcompute.ListContainersRequest{Project: project, LabelSelector: sel, PageToken: page}))
			return msg(r).GetItems(), msg(r).GetNextPageToken(), err
		},
		apply: func(ctx context.Context, o *hangarcompute.Container) (*hangarcompute.Container, error) {
			o.GetMetadata().Project = project
			r, err := cts.ApplyContainer(ctx, connect.NewRequest(&hangarcompute.ApplyContainerRequest{Container: o}))
			return msg(r).GetContainer(), err
		},
		del: func(ctx context.Context, name string) error {
			_, err := cts.DeleteContainer(ctx, connect.NewRequest(&hangarcompute.DeleteContainerRequest{Project: project, Name: name}))
			return err
		},
	}
	vms := computev1alpha1connect.NewVirtualMachineServiceClient(hc, c.endpoint, opts...)
	c.vms = kind[*hangarcompute.VirtualMachine]{
		project: project, what: "virtual machine",
		get: func(ctx context.Context, name string) (*hangarcompute.VirtualMachine, error) {
			r, err := vms.GetVirtualMachine(ctx, connect.NewRequest(&hangarcompute.GetVirtualMachineRequest{Project: project, Name: name}))
			return msg(r).GetVirtualMachine(), err
		},
		list: func(ctx context.Context, sel, page string) ([]*hangarcompute.VirtualMachine, string, error) {
			r, err := vms.ListVirtualMachines(ctx, connect.NewRequest(&hangarcompute.ListVirtualMachinesRequest{Project: project, LabelSelector: sel, PageToken: page}))
			return msg(r).GetItems(), msg(r).GetNextPageToken(), err
		},
		apply: func(ctx context.Context, o *hangarcompute.VirtualMachine) (*hangarcompute.VirtualMachine, error) {
			o.GetMetadata().Project = project
			r, err := vms.ApplyVirtualMachine(ctx, connect.NewRequest(&hangarcompute.ApplyVirtualMachineRequest{VirtualMachine: o}))
			return msg(r).GetVirtualMachine(), err
		},
		del: func(ctx context.Context, name string) error {
			_, err := vms.DeleteVirtualMachine(ctx, connect.NewRequest(&hangarcompute.DeleteVirtualMachineRequest{Project: project, Name: name}))
			return err
		},
	}
	secs := secretsv1alpha1connect.NewSecretServiceClient(hc, c.endpoint, opts...)
	c.secrets = kind[*hangarsecrets.Secret]{
		project: project, what: "secret",
		get: func(ctx context.Context, name string) (*hangarsecrets.Secret, error) {
			r, err := secs.GetSecret(ctx, connect.NewRequest(&hangarsecrets.GetSecretRequest{Project: project, Name: name}))
			return msg(r).GetSecret(), err
		},
		list: func(ctx context.Context, sel, page string) ([]*hangarsecrets.Secret, string, error) {
			r, err := secs.ListSecrets(ctx, connect.NewRequest(&hangarsecrets.ListSecretsRequest{Project: project, LabelSelector: sel, PageToken: page}))
			return msg(r).GetItems(), msg(r).GetNextPageToken(), err
		},
		apply: func(ctx context.Context, o *hangarsecrets.Secret) (*hangarsecrets.Secret, error) {
			o.GetMetadata().Project = project
			r, err := secs.ApplySecret(ctx, connect.NewRequest(&hangarsecrets.ApplySecretRequest{Secret: o}))
			return msg(r).GetSecret(), err
		},
		del: func(ctx context.Context, name string) error {
			_, err := secs.DeleteSecret(ctx, connect.NewRequest(&hangarsecrets.DeleteSecretRequest{Project: project, Name: name}))
			return err
		},
	}
	imgs := storagev1alpha1connect.NewCustomImageServiceClient(hc, c.endpoint, opts...)
	c.images = kind[*hangarstorage.CustomImage]{
		project: project, what: "custom image",
		get: func(ctx context.Context, name string) (*hangarstorage.CustomImage, error) {
			r, err := imgs.GetCustomImage(ctx, connect.NewRequest(&hangarstorage.GetCustomImageRequest{Project: project, Name: name}))
			return msg(r).GetCustomImage(), err
		},
		list: func(ctx context.Context, sel, page string) ([]*hangarstorage.CustomImage, string, error) {
			r, err := imgs.ListCustomImages(ctx, connect.NewRequest(&hangarstorage.ListCustomImagesRequest{Project: project, LabelSelector: sel, PageToken: page}))
			return msg(r).GetItems(), msg(r).GetNextPageToken(), err
		},
		apply: func(ctx context.Context, o *hangarstorage.CustomImage) (*hangarstorage.CustomImage, error) {
			o.GetMetadata().Project = project
			r, err := imgs.ApplyCustomImage(ctx, connect.NewRequest(&hangarstorage.ApplyCustomImageRequest{CustomImage: o}))
			return msg(r).GetCustomImage(), err
		},
		del: func(ctx context.Context, name string) error {
			_, err := imgs.DeleteCustomImage(ctx, connect.NewRequest(&hangarstorage.DeleteCustomImageRequest{Project: project, Name: name}))
			return err
		},
	}
	epsvcsC := networkv1alpha1connect.NewEndpointServiceServiceClient(hc, c.endpoint, opts...)
	c.epsvcs = kind[*hangarnetwork.EndpointService]{
		project: project, what: "endpoint service",
		get: func(ctx context.Context, name string) (*hangarnetwork.EndpointService, error) {
			r, err := epsvcsC.GetEndpointService(ctx, connect.NewRequest(&hangarnetwork.GetEndpointServiceRequest{Project: project, Name: name}))
			return msg(r).GetEndpointService(), err
		},
		list: func(ctx context.Context, sel, page string) ([]*hangarnetwork.EndpointService, string, error) {
			r, err := epsvcsC.ListEndpointServices(ctx, connect.NewRequest(&hangarnetwork.ListEndpointServicesRequest{Project: project, LabelSelector: sel, PageToken: page}))
			return msg(r).GetItems(), msg(r).GetNextPageToken(), err
		},
		apply: func(ctx context.Context, o *hangarnetwork.EndpointService) (*hangarnetwork.EndpointService, error) {
			o.GetMetadata().Project = project
			r, err := epsvcsC.ApplyEndpointService(ctx, connect.NewRequest(&hangarnetwork.ApplyEndpointServiceRequest{EndpointService: o}))
			return msg(r).GetEndpointService(), err
		},
		del: func(ctx context.Context, name string) error {
			_, err := epsvcsC.DeleteEndpointService(ctx, connect.NewRequest(&hangarnetwork.DeleteEndpointServiceRequest{Project: project, Name: name}))
			return err
		},
	}
	nepsC := networkv1alpha1connect.NewNetworkEndpointServiceClient(hc, c.endpoint, opts...)
	c.neps = kind[*hangarnetwork.NetworkEndpoint]{
		project: project, what: "network endpoint",
		get: func(ctx context.Context, name string) (*hangarnetwork.NetworkEndpoint, error) {
			r, err := nepsC.GetNetworkEndpoint(ctx, connect.NewRequest(&hangarnetwork.GetNetworkEndpointRequest{Project: project, Name: name}))
			return msg(r).GetNetworkEndpoint(), err
		},
		list: func(ctx context.Context, sel, page string) ([]*hangarnetwork.NetworkEndpoint, string, error) {
			r, err := nepsC.ListNetworkEndpoints(ctx, connect.NewRequest(&hangarnetwork.ListNetworkEndpointsRequest{Project: project, LabelSelector: sel, PageToken: page}))
			return msg(r).GetItems(), msg(r).GetNextPageToken(), err
		},
		apply: func(ctx context.Context, o *hangarnetwork.NetworkEndpoint) (*hangarnetwork.NetworkEndpoint, error) {
			o.GetMetadata().Project = project
			r, err := nepsC.ApplyNetworkEndpoint(ctx, connect.NewRequest(&hangarnetwork.ApplyNetworkEndpointRequest{NetworkEndpoint: o}))
			return msg(r).GetNetworkEndpoint(), err
		},
		del: func(ctx context.Context, name string) error {
			_, err := nepsC.DeleteNetworkEndpoint(ctx, connect.NewRequest(&hangarnetwork.DeleteNetworkEndpointRequest{Project: project, Name: name}))
			return err
		},
	}
	sgsC := networkv1alpha1connect.NewSecurityGroupServiceClient(hc, c.endpoint, opts...)
	c.sgs = kind[*hangarnetwork.SecurityGroup]{
		project: project, what: "security group",
		get: func(ctx context.Context, name string) (*hangarnetwork.SecurityGroup, error) {
			r, err := sgsC.GetSecurityGroup(ctx, connect.NewRequest(&hangarnetwork.GetSecurityGroupRequest{Project: project, Name: name}))
			return msg(r).GetSecurityGroup(), err
		},
		list: func(ctx context.Context, sel, page string) ([]*hangarnetwork.SecurityGroup, string, error) {
			r, err := sgsC.ListSecurityGroups(ctx, connect.NewRequest(&hangarnetwork.ListSecurityGroupsRequest{Project: project, LabelSelector: sel, PageToken: page}))
			return msg(r).GetItems(), msg(r).GetNextPageToken(), err
		},
		apply: func(ctx context.Context, o *hangarnetwork.SecurityGroup) (*hangarnetwork.SecurityGroup, error) {
			o.GetMetadata().Project = project
			r, err := sgsC.ApplySecurityGroup(ctx, connect.NewRequest(&hangarnetwork.ApplySecurityGroupRequest{SecurityGroup: o}))
			return msg(r).GetSecurityGroup(), err
		},
		del: func(ctx context.Context, name string) error {
			_, err := sgsC.DeleteSecurityGroup(ctx, connect.NewRequest(&hangarnetwork.DeleteSecurityGroupRequest{Project: project, Name: name}))
			return err
		},
	}
	c.machines = computev1alpha1connect.NewMachineServiceClient(hc, c.endpoint, opts...)
	c.globals = storagev1alpha1connect.NewImageServiceClient(hc, c.endpoint, opts...)
	c.projects = tenancyv1alpha1connect.NewProjectServiceClient(hc, c.endpoint, opts...)
	return c
}

func (c *Connect) Networks() Kind[*hangarnetwork.Network]                 { return c.networks }
func (c *Connect) Disks() Kind[*hangarstorage.Disk]                       { return c.disks }
func (c *Connect) Containers() Kind[*hangarcompute.Container]             { return c.containers }
func (c *Connect) VirtualMachines() Kind[*hangarcompute.VirtualMachine]   { return c.vms }
func (c *Connect) Secrets() Kind[*hangarsecrets.Secret]                   { return c.secrets }
func (c *Connect) CustomImages() Kind[*hangarstorage.CustomImage]         { return c.images }
func (c *Connect) ProjectName() string                                    { return c.project }
func (c *Connect) EndpointServices() Kind[*hangarnetwork.EndpointService] { return c.epsvcs }
func (c *Connect) NetworkEndpoints() Kind[*hangarnetwork.NetworkEndpoint] { return c.neps }
func (c *Connect) SecurityGroups() Kind[*hangarnetwork.SecurityGroup]     { return c.sgs }

func (c *Connect) Image(ctx context.Context, name string) (*hangarstorage.Image, error) {
	r, err := c.globals.GetImage(ctx, connect.NewRequest(&hangarstorage.GetImageRequest{Name: name}))
	if err != nil {
		return nil, apiError("image "+name, err)
	}
	return r.Msg.GetImage(), nil
}

func (c *Connect) Machine(ctx context.Context, name string) (*hangarcompute.Machine, error) {
	r, err := c.machines.GetMachine(ctx, connect.NewRequest(&hangarcompute.GetMachineRequest{Project: c.project, Name: name}))
	if err != nil {
		return nil, apiError("machine "+name, err)
	}
	return r.Msg.GetMachine(), nil
}

func (c *Connect) Project(ctx context.Context) (*hangartenancy.Project, error) {
	r, err := c.projects.GetProject(ctx, connect.NewRequest(&hangartenancy.GetProjectRequest{Name: c.project}))
	if err != nil {
		return nil, apiError("project "+c.project, err)
	}
	return r.Msg.GetProject(), nil
}

// bearer sends the token with every call.
func bearer(token string) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, req)
		}
	}
}

// kind is a Kind over one hangar service's calls.
type kind[T any] struct {
	project, what string
	get           func(ctx context.Context, name string) (T, error)
	list          func(ctx context.Context, selector, page string) ([]T, string, error)
	apply         func(ctx context.Context, obj T) (T, error)
	del           func(ctx context.Context, name string) error
}

func (k kind[T]) Get(ctx context.Context, name string) (T, error) {
	o, err := k.get(ctx, name)
	if err != nil {
		var zero T
		return zero, apiError(k.what+" "+name, err)
	}
	return o, nil
}

func (k kind[T]) List(ctx context.Context, selector string) ([]T, error) {
	var all []T
	page := ""
	for {
		items, next, err := k.list(ctx, selector, page)
		if err != nil {
			return nil, apiError("listing "+k.what+"s", err)
		}
		all = append(all, items...)
		if next == "" {
			return all, nil
		}
		page = next
	}
}

func (k kind[T]) Apply(ctx context.Context, obj T) (T, error) {
	o, err := k.apply(ctx, obj)
	if err != nil {
		var zero T
		return zero, apiError("applying a "+k.what, err)
	}
	return o, nil
}

func (k kind[T]) Delete(ctx context.Context, name string) error {
	if err := k.del(ctx, name); err != nil && connect.CodeOf(err) != connect.CodeNotFound {
		return apiError("deleting "+k.what+" "+name, err)
	}
	return nil
}

// apiError says what failed, and is ErrNotFound for what is not there.
func apiError(what string, err error) error {
	if connect.CodeOf(err) == connect.CodeNotFound {
		return fmt.Errorf("%s: %w: %w", what, err, ErrNotFound)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// msg is a call's answer, nil for a call that failed.
func msg[T any](r *connect.Response[T]) *T {
	if r == nil {
		return nil
	}
	return r.Msg
}
