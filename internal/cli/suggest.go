package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
)

// closest returns the candidate nearest to s, ignoring case, if it is near
// enough to be a likely typo of it.
func closest(s string, candidates []string) (string, bool) {
	s = strings.ToLower(s)
	best, bestDist, bestPrefix := "", -1, 0
	for _, c := range candidates {
		lc := strings.ToLower(c)
		d, p := editDistance(s, lc), commonPrefix(s, lc)
		// Of names as near, the one that starts as s does: vms is vm, not ms.
		if bestDist < 0 || d < bestDist || d == bestDist && p > bestPrefix {
			best, bestDist, bestPrefix = c, d, p
		}
	}
	// A third of the length, one edit at least: past that the nearest name
	// is seldom the one meant.
	limit := max(1, len(s)/3)
	return best, bestDist >= 0 && bestDist <= limit
}

// editDistance is how many inserts, deletes, substitutions and swaps of
// neighbours turn a into b, in bytes; a swap counts as one, as in prot for
// port.
func editDistance(a, b string) int {
	d := make([][]int, len(a)+1)
	for i := range d {
		d[i] = make([]int, len(b)+1)
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(a)][len(b)]
}

func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// kindNames lists what a command line can name each kind by, so the closest
// one to a typo can be found.
func kindNames() (names []string, of map[string]registry.Resource) {
	of = map[string]registry.Resource{}
	for _, r := range registry.All {
		for _, n := range append([]string{strings.ToLower(r.Kind), r.Plural}, r.ShortNames...) {
			names = append(names, n)
			of[n] = r
		}
	}
	return names, of
}

// knownKinds is the line that lists every kind by its plural, with its short
// names, as get and delete take them.
func knownKinds() string {
	var parts []string
	for _, r := range registry.All {
		p := r.Plural
		if len(r.ShortNames) > 0 {
			p += " (" + strings.Join(r.ShortNames, ", ") + ")"
		}
		parts = append(parts, p)
	}
	slices.Sort(parts)
	return "kinds: " + strings.Join(parts, ", ")
}

// unknownKind is the error for a kind named on the command line that none is.
func unknownKind(name string) error {
	names, of := kindNames()
	msg := fmt.Sprintf("unknown kind %q", name)
	if n, ok := closest(name, names); ok {
		msg += fmt.Sprintf("; did you mean %s?", of[n].Plural)
	}
	return usageErr("%s\n%s", msg, knownKinds())
}

// unknownManifestKind is the error for a manifest whose apiVersion and kind
// name no kind: it says which of the two is wrong, and what is right.
func unknownManifestKind(apiVersion, kind string) error {
	if kind == "" || apiVersion == "" {
		return usageErr("apiVersion and kind are required, such as apiVersion: %s and kind: %s\n%s",
			registry.Box.APIVersion(), registry.Box.Kind, manifestKinds())
	}
	for _, r := range registry.All {
		if r.Kind == kind {
			return usageErr("kind %s is in apiVersion %s, not %q", kind, r.APIVersion(), apiVersion)
		}
	}
	var kinds []string
	for _, r := range registry.All {
		kinds = append(kinds, r.Kind)
	}
	msg := fmt.Sprintf("unknown kind %q", kind)
	if k, ok := closest(kind, kinds); ok {
		msg += fmt.Sprintf("; did you mean %s?", k)
	}
	return usageErr("%s\n%s", msg, manifestKinds())
}

// manifestKinds lists every kind as a manifest names it, by its apiVersion.
func manifestKinds() string {
	var b strings.Builder
	b.WriteString("kinds by apiVersion:")
	last := ""
	for _, r := range registry.All {
		if v := r.APIVersion(); v != last {
			b.WriteString("\n  " + v + ":")
			last = v
		} else {
			b.WriteString(",")
		}
		b.WriteString(" " + r.Kind)
	}
	return b.String()
}
