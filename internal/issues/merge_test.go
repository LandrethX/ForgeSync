package issues

import (
	"strings"
	"testing"
)

func TestMerge(t *testing.T) {
	cases := []struct {
		name     string
		vals     map[string]string
		base     string
		value    string
		writes   string
		conflict bool
	}{
		{"all agree", map[string]string{"se": "a", "dk": "a"}, "a", "a", "", false},
		{"changed on the primary", map[string]string{"se": "b", "dk": "a", "de": "a"}, "a", "b", "de,dk", false},
		{"changed on a replica: taken over everywhere", map[string]string{"se": "a", "dk": "b", "de": "a"}, "a", "b", "de,se", false},
		{"same change on two nodes", map[string]string{"se": "b", "dk": "b", "de": "a"}, "a", "b", "de", false},
		{"different changes: conflict", map[string]string{"se": "b", "dk": "c", "de": "a"}, "a", "", "", true},
		{"a new copy's empty base agrees", map[string]string{"se": "x"}, "x", "x", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			value, writes, conflict := merge(c.vals, c.base)
			if value != c.value || strings.Join(writes, ",") != c.writes || conflict != c.conflict {
				t.Errorf("merge = %q %v %v, want %q [%s] %v", value, writes, conflict, c.value, c.writes, c.conflict)
			}
		})
	}
}
