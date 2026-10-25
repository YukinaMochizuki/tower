// Package labels parses and evaluates label selectors: a comma-separated list
// of requirements, each "key=value", "key!=value", "key" (present) or "!key"
// (absent). All requirements must hold.
package labels

import (
	"fmt"
	"strings"
)

type op int

const (
	equals op = iota
	notEquals
	exists
	notExists
)

type requirement struct {
	key, value string
	op         op
}

// Selector matches label sets. The zero Selector matches everything.
type Selector []requirement

// Parse parses a selector; the empty string selects everything.
func Parse(s string) (Selector, error) {
	var sel Selector
	if strings.TrimSpace(s) == "" {
		return sel, nil
	}
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		var r requirement
		switch {
		case strings.Contains(part, "!="):
			k, v, _ := strings.Cut(part, "!=")
			r = requirement{key: strings.TrimSpace(k), value: strings.TrimSpace(v), op: notEquals}
		case strings.Contains(part, "="):
			k, v, _ := strings.Cut(part, "=")
			r = requirement{key: strings.TrimSpace(k), value: strings.TrimSpace(strings.TrimPrefix(v, "=")), op: equals}
		case strings.HasPrefix(part, "!"):
			r = requirement{key: strings.TrimSpace(part[1:]), op: notExists}
		default:
			r = requirement{key: part, op: exists}
		}
		if r.key == "" {
			return nil, fmt.Errorf("label selector %q: empty key in %q", s, part)
		}
		sel = append(sel, r)
	}
	return sel, nil
}

// Matches reports whether the labels satisfy every requirement.
func (s Selector) Matches(labels map[string]string) bool {
	for _, r := range s {
		v, ok := labels[r.key]
		switch r.op {
		case equals:
			if !ok || v != r.value {
				return false
			}
		case notEquals:
			if ok && v == r.value {
				return false
			}
		case exists:
			if !ok {
				return false
			}
		case notExists:
			if ok {
				return false
			}
		}
	}
	return true
}
