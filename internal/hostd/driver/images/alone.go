package images

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// solveFunc solves opt with BuildKit, writing its progress to status,
// which it closes as it returns, as client.Solve does.
type solveFunc func(ctx context.Context, c *client.Client, opt client.SolveOpt, status chan *client.SolveStatus) (*client.SolveResponse, error)

func buildkitSolve(ctx context.Context, c *client.Client, opt client.SolveOpt, status chan *client.SolveStatus) (*client.SolveResponse, error) {
	return c.Solve(ctx, nil, opt, status)
}

// solveQuoting solves opt, calls after as the solve returns, and returns
// the digest of the image it exported. A failure quotes the end of what
// the build's steps wrote: a RUN that failed says why there, not in
// BuildKit's error.
func (d *Driver) solveQuoting(ctx context.Context, c *client.Client, opt client.SolveOpt, repository string, after func()) (string, error) {
	status := make(chan *client.SolveStatus)
	logged := make(chan string, 1)
	go func() {
		var tail []byte
		for s := range status {
			for _, l := range s.Logs {
				tail = append(tail, l.Data...)
				if len(tail) > 2*maxQuoted {
					tail = tail[len(tail)-maxQuoted:]
				}
			}
		}
		if len(tail) > maxQuoted {
			tail = tail[len(tail)-maxQuoted:]
		}
		logged <- strings.TrimSpace(string(tail))
	}()
	resp, err := d.solve(ctx, c, opt, status)
	after()
	tail := <-logged // the solve closes status as it returns
	if err != nil {
		if strings.Contains(err.Error(), "no space left") {
			return "", fmt.Errorf("building %s: %w: %w", repository, err, driver.ErrNoSpace)
		}
		if tail != "" {
			return "", fmt.Errorf("building %s: %w; it last wrote: %s", repository, err, tail)
		}
		return "", fmt.Errorf("building %s: %w", repository, err)
	}
	dgst := resp.ExporterResponse[exptypes.ExporterImageDigestKey]
	if dgst == "" {
		return "", fmt.Errorf("building %s: BuildKit named no digest", repository)
	}
	return dgst, nil
}

// pushExports is what a build without Docker exports: the image, pushed by
// BuildKit to the registry, over plain HTTP if it is served so, as name
// with a tag of the build's own. Pushed untagged, it would take the tag
// latest from the build before it, and a registry that collects untagged
// manifests would take that one; what goes from the registry is tower's
// to decide.
func (d *Driver) pushExports(name string) ([]client.ExportEntry, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return []client.ExportEntry{{Type: client.ExporterImage, Attrs: map[string]string{
		string(exptypes.OptKeyName):     name + ":" + buildTagPrefix + hex.EncodeToString(b),
		string(exptypes.OptKeyPush):     "true",
		string(exptypes.OptKeyInsecure): strconv.FormatBool(d.cfg.PlainHTTP),
		"rewrite-timestamp":             "true",
	}}}, nil
}

// buildTagPrefix begins the tag each build without Docker is pushed with.
const buildTagPrefix = "build-"
