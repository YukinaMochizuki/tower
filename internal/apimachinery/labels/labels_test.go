package labels_test

import (
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/apimachinery/labels"
)

func TestMatches(t *testing.T) {
	set := map[string]string{"app": "web", "tier": "front"}
	tests := []struct {
		selector string
		want     bool
	}{
		{"", true},
		{"app=web", true},
		{"app==web", true},
		{"app=db", false},
		{"app!=db", true},
		{"app!=web", false},
		{"missing!=x", true},
		{"tier", true},
		{"missing", false},
		{"!missing", true},
		{"!app", false},
		{"app=web, tier=front", true},
		{"app=web,tier=back", false},
	}
	for _, tt := range tests {
		sel, err := labels.Parse(tt.selector)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tt.selector, err)
		}
		if got := sel.Matches(set); got != tt.want {
			t.Errorf("%q matches = %t, want %t", tt.selector, got, tt.want)
		}
	}
}

func TestParseRejectsEmptyKeys(t *testing.T) {
	for _, s := range []string{"=x", "a=b,", "!", "!=x"} {
		if _, err := labels.Parse(s); err == nil {
			t.Errorf("Parse(%q) succeeded", s)
		}
	}
}
