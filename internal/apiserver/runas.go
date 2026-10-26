package apiserver

import (
	"context"
	"regexp"

	"github.com/YukinaMochizuki/tower/internal/provider"
)

// posixUser is a portable user name, as useradd takes by default: lower
// case letters, digits, '_' and '-', starting with a letter or '_', at most
// 32 long.
var posixUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// asUser is command run as user, or as root when user is empty. runuser
// (util-linux) sets HOME, SHELL, USER and LOGNAME as the user's and keeps
// the rest of the environment, the working directory among it, so a
// configure phase's secrets, given as its environment, reach the
// playbook; sudo would drop them.
func asUser(user string, command ...string) []string {
	if user == "" {
		return command
	}
	return append([]string{"runuser", "-u", user, "--"}, command...)
}

// prepareUserScript makes the user $1 if the VM has none of the name,
// with a home directory and a group of its own; gives it a home if it
// lacks one, and passwordless sudo, which a playbook's become tasks reach
// root through; and hands it the files after it, which tower wrote as
// root. It changes nothing a second time: it runs again in each clone of
// a golden, which has the user already. The sudoers rule is written
// under a name with a dot first, which sudo skips, so a half-written one
// is never read.
const prepareUserScript = `set -eu
u=$1; shift
command -v sudo >/dev/null 2>&1 || { echo "the VM has no sudo, which a run_as user needs" >&2; exit 1; }
if ! id -u "$u" >/dev/null 2>&1; then
	sh=/bin/sh
	[ -x /bin/bash ] && sh=/bin/bash
	useradd --create-home --user-group --shell "$sh" "$u"
fi
home=$(getent passwd "$u" | cut -d: -f6)
[ -d "$home" ] || install -d -m 0750 -o "$u" -g "$(id -gn "$u")" "$home"
printf '%s ALL=(ALL:ALL) NOPASSWD:ALL\n' "$u" >/etc/sudoers.d/.tower-run-as
chmod 0440 /etc/sudoers.d/.tower-run-as
mv -f /etc/sudoers.d/.tower-run-as /etc/sudoers.d/tower-run-as
[ $# = 0 ] || chown "$u:$(id -gn "$u")" "$@"
`

// prepareUser runs prepareUserScript as root for user, handing it files.
// Nothing to do for root (user empty).
func (s *Server) prepareUser(ctx context.Context, p provider.Provider, ref provider.Instance, user string, files ...string) error {
	if user == "" {
		return nil
	}
	return s.mustRun(ctx, p, ref, nil, "/", append([]string{"sh", "-c", prepareUserScript, "sh", user}, files...)...)
}
