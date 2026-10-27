// Package system is a hangar target's system container: tower-system
// (cmd/tower-system), the program in its image, which keeps the target's
// git mirrors and builds its images (docs/decisions/repositories.md,
// images.md), and Ops, the provider's side of it. As tower-guest's shim
// does (internal/guest/guestbin), it runs one operation a call, named by a
// header on its stdin, the call's data in frames after it; it is reached
// by hangar's exec. What a call needs of a secret (a repository's token, a
// registry login, a tailnet's auth key) is in its header's Env alone, for
// that call: nothing keeps it, but a grant's, which the server keeps until
// it is used or expires.
//
// Beside the calls, the image runs `tower-system serve` (server.go): what a
// box takes of a mirror, or pushes, goes between the box's tower-guest and
// it directly, over hangar's EndpointService, on a single-use credential
// tower grants it through a call (docs/decisions/traffic.md).
//
// The mirrors are hostd's git driver's (internal/hostd/driver/gitmirror),
// in a directory of the container's disk; builds are its image driver's
// (internal/hostd/driver/images), through the container's buildkitd,
// pushed to tower's registry over the tailnet the container joins with an
// identity of its own.
package system

import (
	"encoding/json"
	"io"

	"github.com/YukinaMochizuki/tower/internal/guest/guestbin"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// Header is a call's operation.
type Header struct {
	Op string `json:"op"`
	// The mirror's operations: its name, the repository's URL and branch,
	// a ref (a branch, tag or commit), a commit, a push's lease, and the
	// paths a read reads, with the most of a file's content it gives.
	Mirror     string   `json:"mirror,omitempty"`
	URL        string   `json:"url,omitempty"`
	Branch     string   `json:"branch,omitempty"`
	Ref        string   `json:"ref,omitempty"`
	Commit     string   `json:"commit,omitempty"`
	Lease      string   `json:"lease,omitempty"`
	Paths      []string `json:"paths,omitempty"`
	MaxContent int64    `json:"maxContent,omitempty"`
	// Build: the repository the image goes to, the context's directory and
	// the Dockerfile's path, of the context its data is or, with Mirror,
	// of the mirror's work tree at Ref. Resolve: the image.
	Repository string `json:"repository,omitempty"`
	Dir        string `json:"dir,omitempty"`
	Dockerfile string `json:"dockerfile,omitempty"`
	Image      string `json:"image,omitempty"`
	// Hostname is the name the container joins the tailnet as.
	Hostname string `json:"hostname,omitempty"`
	// Grant: the operation granted (GrantCheckout, GrantOrigin or
	// GrantPush), the workstream network the box asking it is on, and how
	// long the credential holds, in seconds; Sources, with it the network's
	// own, and alone in a sources call every network's.
	Grant   string   `json:"grant,omitempty"`
	Network string   `json:"network,omitempty"`
	TTL     int      `json:"ttl,omitempty"`
	Sources []Source `json:"sources,omitempty"`
	// Env holds the call's secrets, by the names below.
	Env map[string]string `json:"env,omitempty"`
}

// Source is a workstream network as the server sees it: what reaches the
// server through the network's NetworkEndpoint comes from Address, the
// network's transit address, and the network is the tower project's
// Project.
type Source struct {
	Network string `json:"network"`
	Project string `json:"project"`
	Address string `json:"address"`
}

// The operations.
const (
	OpFetch     = "fetch"
	OpCheckout  = "checkout"
	OpOrigin    = "origin"
	OpPush      = "push"
	OpBehind    = "behind"
	OpRead      = "read"
	OpBuild     = "build"
	OpResolve   = "resolve"
	OpStatus    = "status"
	OpTailnetUp = "tailnet-up"
	// OpGrant registers a credential with the server; OpSources gives it
	// every workstream network's Source, keeping those of the networks a
	// live grant is for.
	OpGrant   = "grant"
	OpSources = "sources"
	// OpRevoke takes back a credential not presented.
	OpRevoke = "revoke"
)

// The operations a grant is for, each the server's path of its own.
const (
	GrantCheckout = "checkout"
	GrantOrigin   = "origin"
	GrantPush     = "push"
)

// Names of Header.Env.
const (
	EnvToken            = "TOKEN"
	EnvRegistryUsername = "REGISTRY_USERNAME"
	EnvRegistryPassword = "REGISTRY_PASSWORD"
	EnvTailnetAuthKey   = "TAILNET_AUTH_KEY"
	// EnvCredential is a grant's credential.
	EnvCredential = "CREDENTIAL"
)

// Exit codes of a call that failed, its message on stderr.
const (
	ExitNotFound  = 200
	ExitLeaseLost = 201
	ExitFailed    = 202
	ExitNoSpace   = 203
)

// Result is what a call answers, a line of JSON on its stdout; a checkout
// writes its tar after it.
type Result struct {
	// Fetch's head, a checkout's commit, a read's commit.
	Commit string `json:"commit,omitempty"`
	// Behind's count.
	Behind int `json:"behind,omitempty"`
	// A read's entries, by path.
	Entries map[string]driver.MirrorEntry `json:"entries,omitempty"`
	// A build's image, a resolve's digest and user.
	Image  string `json:"image,omitempty"`
	Digest string `json:"digest,omitempty"`
	User   string `json:"user,omitempty"`
	// Status: whether buildkitd answers, with its version, and whether the
	// container is on the tailnet.
	BuildKit string `json:"buildkit,omitempty"`
	Tailnet  bool   `json:"tailnet,omitempty"`
	// Serving says the server answers on its control socket.
	Serving bool `json:"serving,omitempty"`
}

// writeHeader writes a call's header frame.
func writeHeader(w io.Writer, h Header) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	return guestbin.WriteFrame(w, guestbin.FrameHeader, b)
}

// readHeader reads a call's header frame.
func readHeader(r io.Reader) (Header, error) {
	var h Header
	typ, data, err := guestbin.ReadFrame(r)
	if err != nil {
		return h, err
	}
	if typ != guestbin.FrameHeader {
		return h, errNoHeader
	}
	return h, json.Unmarshal(data, &h)
}
