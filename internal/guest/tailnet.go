package guest

// A container that joins a tailnet runs its own tailscaled, which tower-guest
// init starts before the image's entrypoint; everything else in it runs
// without NET_ADMIN (docs/decisions/tailnet.md). These are the paths and
// variables hostd's container driver and tower-guest share for it.
const (
	// TailnetDir holds the node's state and how its join stands
	// (TailnetStatusFile), tower's, apart from /workspace: a copy of the
	// workspace must not copy the node.
	TailnetDir = "/.tower/tailnet"
	// TailnetStatusFile, in TailnetDir, is a TailnetStatus as JSON.
	TailnetStatusFile = "status.json"
	// TailnetStateFile, in TailnetDir, is tailscaled's state: a node with
	// none has nothing to log out.
	TailnetStateFile = "tailscaled.state"
	// TailnetControlDir is the driver's, bound read-only into the
	// container: the box cannot write it. TailnetLeaveFile there says to
	// init to log the node out and exit, starting nothing of the image's.
	TailnetControlDir = "/.tower/tailnet-control"
	TailnetLeaveFile  = "leave"
	// Tailscaled and Tailscale are tailscale's daemon and its CLI, bound
	// read-only into the container as tower-guest is.
	Tailscaled = "/.tower/tailscaled"
	Tailscale  = "/.tower/tailscale"
	// TailscaleSocket is tailscaled's, where the image's own tailscale CLI
	// looks for it.
	TailscaleSocket = "/var/run/tailscale/tailscaled.sock"
	// RunAsEnv, in the environment of a tower-guest shim started as root,
	// says to drop NET_ADMIN and run as the user it names, as Docker names
	// one (name or uid, with :group or :gid); empty is root. RunAsHomeEnv,
	// "1", says to set HOME to that user's, which the image's own
	// environment does not set.
	RunAsEnv     = "TOWER_GUEST_RUN_AS"
	RunAsHomeEnv = "TOWER_GUEST_RUN_AS_HOME"
)

// TailnetStatus is how a container's join to its tailnet stands, as
// tower-guest last saw it: a reason of provider's TailnetJoined, and words
// for people, which never hold the key.
type TailnetStatus struct {
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}
