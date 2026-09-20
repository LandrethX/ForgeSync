// Package health monitors Forgejo nodes.
//
// A node counts as healthy only if Forgejo's health endpoint passes and the
// ForgeSync token still authenticates as the expected service account. A
// failed contact first makes the node SUSPECT; only FailureThreshold
// consecutive failures make it UNREACHABLE, so short network blips don't
// look like outages.
package health

import (
	"time"
)

// State is what ForgeSync believes about one node: healthy, suspect after
// a failed contact, unreachable after failure_threshold of them, or
// unknown before the first.
type State string

const (
	Unknown     State = "UNKNOWN"
	Healthy     State = "HEALTHY"
	Degraded    State = "DEGRADED"    // reachable, but Forgejo reports a problem
	Suspect     State = "SUSPECT"     // unreachable, below the failure threshold
	Unreachable State = "UNREACHABLE" // unreachable, threshold reached
	AuthError   State = "AUTH_ERROR"  // reachable, but the token is rejected or belongs to the wrong account
)

// Valid reports whether s is one of the states, so a value read back from
// the database (or an older version's) can't become the current state.
func (s State) Valid() bool {
	switch s {
	case Unknown, Healthy, Degraded, Suspect, Unreachable, AuthError:
		return true
	}
	return false
}

// Status is the current view of one node.
type Status struct {
	Node                string     `json:"node"`
	State               State      `json:"state"`
	Version             string     `json:"version,omitempty"`
	LastChecked         time.Time  `json:"last_checked"`
	LastSeen            *time.Time `json:"last_seen,omitempty"`     // last successful contact
	FailingSince        *time.Time `json:"failing_since,omitempty"` // when the node left HEALTHY
	ConsecutiveFailures int        `json:"consecutive_failures"`
	LastError           string     `json:"last_error,omitempty"`
}

// Result is the outcome of one round of checks.
type Result struct {
	Reachable bool   // Forgejo answered at all
	ForgejoOK bool   // /api/healthz passed
	AuthOK    bool   // token valid and belongs to the service account
	Version   string // empty if unknown
	Err       error  // first problem found, if any
}

// Next applies a check result to the previous status.
func Next(prev Status, r Result, threshold int, now time.Time) Status {
	s := prev
	s.LastChecked = now
	s.LastError = ""
	if r.Err != nil {
		s.LastError = r.Err.Error()
	}

	if !r.Reachable {
		s.ConsecutiveFailures++
		s.State = Suspect
		if s.ConsecutiveFailures >= threshold {
			s.State = Unreachable
		}
	} else {
		s.ConsecutiveFailures = 0
		s.LastSeen = &now
		if r.Version != "" {
			s.Version = r.Version
		}
		switch {
		case !r.AuthOK:
			s.State = AuthError
		case !r.ForgejoOK:
			s.State = Degraded
		default:
			s.State = Healthy
		}
	}

	switch {
	case s.State == Healthy:
		s.FailingSince = nil
	case s.FailingSince == nil:
		s.FailingSince = &now
	}
	return s
}
