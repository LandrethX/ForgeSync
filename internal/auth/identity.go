// Package auth handles who is using the admin UI and API: SceneID (OIDC)
// sign-in and the ForgeSync roles derived from it.
package auth

import (
	"fmt"
	"strings"
)

// Role is a ForgeSync permission level. Higher roles include lower ones.
type Role int

const (
	NoRole Role = iota
	Viewer
	Operator
	Administrator
)

func (r Role) String() string {
	switch r {
	case Viewer:
		return "viewer"
	case Operator:
		return "operator"
	case Administrator:
		return "administrator"
	}
	return "none"
}

func (r Role) MarshalText() ([]byte, error) { return []byte(r.String()), nil }

// UnmarshalText reads a role back, so an identity can be stored as JSON
// and returned as itself.
func (r *Role) UnmarshalText(b []byte) error {
	role, err := ParseRole(string(b))
	if err != nil {
		return err
	}
	*r = role
	return nil
}

// Identity is a signed-in user, or the admin token.
type Identity struct {
	Subject  string `json:"subject"`  // SceneID sub, or "token"
	Username string `json:"username"` // preferred_username or nickname
	Name     string `json:"name"`
	Email    string `json:"email,omitempty"`
	Role     Role   `json:"role"`
	// Source is "sceneid" (a SceneID user), "account" (one of ForgeSync's
	// own, in its database) or "web-token"/"token" (the admin token).
	Source string `json:"source"`
}

// Actor is how the identity appears in the audit log: the source and who,
// so a line says which kind of sign-in did the thing as well as whose.
func (id Identity) Actor() string {
	switch id.Source {
	case "sceneid":
		return "sceneid:" + id.Username
	case "account":
		return "account:" + id.Username
	}
	return id.Source
}

// RoleMapping turns SceneID role/group values into a ForgeSync role.
type RoleMapping struct {
	// Claim is the ID token claim holding the values; dots walk into nested
	// objects, e.g. "realm_access.roles".
	Claim         string
	Administrator []string
	Operator      []string
	Viewer        []string
}

// RoleFor returns the highest role any of the claim's values grants.
func (m RoleMapping) RoleFor(claims map[string]any) Role {
	values := claimValues(claims, m.Claim)
	has := func(want []string) bool {
		for _, w := range want {
			for _, v := range values {
				if v == w {
					return true
				}
			}
		}
		return false
	}
	switch {
	case has(m.Administrator):
		return Administrator
	case has(m.Operator):
		return Operator
	case has(m.Viewer):
		return Viewer
	}
	return NoRole
}

// claimValues reads a string or list-of-strings claim at a dotted path.
func claimValues(claims map[string]any, path string) []string {
	var cur any = claims
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	switch v := cur.(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// ParseRole parses "viewer", "operator" or "administrator".
func ParseRole(s string) (Role, error) {
	for _, r := range []Role{Viewer, Operator, Administrator} {
		if s == r.String() {
			return r, nil
		}
	}
	return NoRole, fmt.Errorf("unknown role %q", s)
}
