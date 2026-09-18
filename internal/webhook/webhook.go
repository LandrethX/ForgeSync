// Package webhook makes Forgejo tell ForgeSync about changes as they happen,
// with stock Forgejo's system webhooks, so replication doesn't wait for the
// next inventory scan.
//
// Installer keeps exactly one ForgeSync system webhook on each node, for the
// push, create, delete and repository events. Each node signs with its own
// secret, derived from the configured one, so a node can't pose as another.
// The hook URL carries a fingerprint of that secret, so a changed secret
// shows up as a different URL and the hook is replaced.
//
// Receiver checks the signature, ignores ForgeSync's own writes, answers at
// once and hands the change to a Dispatcher. Scans keep running on a longer
// interval as a safety net: deliveries can be lost, and some changes (labels,
// milestones, settings, collaborators, renames) send no webhook at all.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// Events are the webhook events ForgeSync subscribes to.
var Events = []string{"push", "create", "delete", "repository", "issues", "issue_comment"}

// IssueEvents are the events that concern issues, not git.
var IssueEvents = map[string]bool{"issues": true, "issue_comment": true}

// NodeSecret is the secret a node signs its deliveries with.
func NodeSecret(master, node string) string {
	m := hmac.New(sha256.New, []byte(master))
	m.Write([]byte("forgesync webhook secret for node " + node))
	return hex.EncodeToString(m.Sum(nil))
}

// HookURL is the URL a node delivers to: base/<node>?v=<secret fingerprint>.
func HookURL(base, node, secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return strings.TrimRight(base, "/") + "/" + node + "?v=" + hex.EncodeToString(sum[:])[:12]
}

// Status is one node's webhook, as last checked and used.
type Status struct {
	Node          string     `json:"node"`
	Installed     bool       `json:"installed"`
	HookID        int64      `json:"hook_id,omitempty"`
	Error         string     `json:"error,omitempty"`
	CheckedAt     *time.Time `json:"checked_at,omitempty"`
	Deliveries    int64      `json:"deliveries"`
	Rejected      int64      `json:"rejected"` // bad signature or unknown node
	LastDelivery  *time.Time `json:"last_delivery_at,omitempty"`
	LastEvent     string     `json:"last_event,omitempty"`
	LastRepoEvent string     `json:"last_repository,omitempty"`
}

// Tracker holds each node's Status; safe for concurrent use.
type Tracker struct {
	mu    sync.Mutex
	nodes map[string]*Status
	order []string
}

func NewTracker(nodes []string) *Tracker {
	t := &Tracker{nodes: map[string]*Status{}, order: nodes}
	for _, n := range nodes {
		t.nodes[n] = &Status{Node: n}
	}
	return t
}

func (t *Tracker) update(node string, f func(*Status)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.nodes[node]; ok {
		f(s)
	}
}

// Snapshot returns every node's status, in configuration order.
func (t *Tracker) Snapshot() []Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Status, 0, len(t.order))
	for _, n := range t.order {
		out = append(out, *t.nodes[n])
	}
	return out
}
