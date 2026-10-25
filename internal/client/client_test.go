package client_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/tower/internal/client"
)

func project(name string) *tenancyv1alpha1.Project {
	return &tenancyv1alpha1.Project{
		Metadata: &metav1.ObjectMeta{Name: name}, Spec: &tenancyv1alpha1.ProjectSpec{DisplayName: name},
	}
}

// TestListAllReturnsTheFirstPagesRevision: a write between two pages must not
// be lost to a watch started from the revision ListAll returns.
func TestListAllReturnsTheFirstPagesRevision(t *testing.T) {
	env := apiservertest.Start(t, nil)
	env.Setup()
	ctx := t.Context()
	root := env.As(apiservertest.Admin)
	for _, name := range []string{"p1", "p2"} {
		if _, err := root.Apply(ctx, registry.Project, project(name)); err != nil {
			t.Fatal(err)
		}
	}

	var late int64
	var once sync.Once
	writeAfterFirstPage := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			res, err := next(ctx, req)
			if err == nil && strings.HasSuffix(req.Spec().Procedure, "/ListProjects") {
				once.Do(func() {
					r, err := root.Apply(ctx, registry.Project, project("late"))
					if err != nil {
						t.Error(err)
						return
					}
					late = r.Object.(*tenancyv1alpha1.Project).GetMetadata().GetResourceVersion()
				})
			}
			return res, err
		}
	})
	paging := env.As(apiservertest.Admin, connect.WithInterceptors(writeAfterFirstPage))
	_, rev, err := paging.ListAll(ctx, registry.Project, client.ListOptions{PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if late == 0 || rev >= late {
		t.Errorf("ListAll returned revision %d; a project written after its first page is at %d", rev, late)
	}
}
