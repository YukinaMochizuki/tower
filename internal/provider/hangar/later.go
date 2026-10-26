package hangar

import (
	"context"
	"fmt"

	"github.com/YukinaMochizuki/tower/internal/provider"
)

// What a hangar target does not do: mirrors, image builds and goldens.
// Each fails as work that ran and failed, saying so.

// errLater is the error of what a hangar target does not do yet.
func errLater(what string) error {
	return fmt.Errorf("a hangar target does not %s yet: %w", what, provider.ErrFailed)
}

func (t *Target) BuildImage(context.Context, provider.BuildRequest) (string, error) {
	return "", errLater("build images")
}

func (t *Target) ResolveImage(context.Context, string) (provider.ImageConfig, error) {
	return provider.ImageConfig{}, errLater("resolve images")
}

func (t *Target) Capture(ctx context.Context, in provider.Instance) (provider.Artifact, error) {
	if _, err := t.find(ctx, in); err != nil {
		return provider.Artifact{}, err
	}
	return provider.Artifact{}, errLater("capture goldens")
}

// DeleteArtifact deletes nothing: a hangar target makes no artifact yet,
// so the one named is not there, which is no error.
func (t *Target) DeleteArtifact(context.Context, string) error { return nil }

func (t *Target) Fetch(context.Context, provider.FetchRequest) (string, error) {
	return "", errLater("keep mirrors")
}

func (t *Target) Checkout(context.Context, provider.Instance, string, string, string) (string, error) {
	return "", errLater("check mirrors out")
}

func (t *Target) UpdateOrigin(context.Context, provider.Instance, string, string) error {
	return errLater("keep mirrors")
}

func (t *Target) CommitsBehind(context.Context, string, string, string) (int, error) {
	return 0, errLater("keep mirrors")
}

func (t *Target) ReadMirror(context.Context, provider.MirrorReadRequest) (provider.MirrorRead, error) {
	return provider.MirrorRead{}, errLater("keep mirrors")
}

func (t *Target) Push(context.Context, provider.PushRequest) error {
	return errLater("push")
}
