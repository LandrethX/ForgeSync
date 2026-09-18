package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Change is what a delivery says happened.
type Change struct {
	Node       string
	Event      string // push, create, delete or repository
	Action     string // for repository: created, deleted
	Repository string // owner/name on that node
}

// Dispatcher acts on a change: replicate the repository, or look at it
// again first if ForgeSync doesn't know it yet.
type Dispatcher interface {
	Changed(ctx context.Context, c Change)
}

// Receiver serves POST <base>/{node}.
type Receiver struct {
	Secret string
	// ServiceUsers maps each node to ForgeSync's account there; changes it
	// made itself are ignored (Phase 0 p05: pushes and API writes show it as
	// the pusher/sender).
	ServiceUsers map[string]string
	Dispatch     Dispatcher
	Tracker      *Tracker
	Log          *slog.Logger
	// Context is what dispatching runs under (the controller's lifetime).
	Context context.Context
}

// maxBody bounds a delivery; a push of many commits lists them all.
const maxBody = 25 << 20

type payload struct {
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Pusher struct {
		Login string `json:"login"`
	} `json:"pusher"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}

// ServeHTTP handles one delivery for the node named by the last path segment.
func (rc *Receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	node := r.PathValue("node")
	if node == "" {
		node = r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	}
	service, known := rc.ServiceUsers[node]
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		http.Error(w, "body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	sig := r.Header.Get("X-Forgejo-Signature")
	if sig == "" {
		sig = r.Header.Get("X-Gitea-Signature")
	}
	if !known || !validSignature(NodeSecret(rc.Secret, node), body, sig) {
		rc.Tracker.update(node, func(s *Status) { s.Rejected++ })
		rc.Log.Warn("rejected a webhook delivery", "node", node, "known_node", known, "remote", r.RemoteAddr)
		http.Error(w, "unknown node or bad signature", http.StatusUnauthorized)
		return
	}
	event := r.Header.Get("X-Forgejo-Event")
	if event == "" {
		event = r.Header.Get("X-Gitea-Event")
	}
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "bad JSON", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	rc.Tracker.update(node, func(s *Status) {
		s.Deliveries++
		s.LastDelivery, s.LastEvent, s.LastRepoEvent = &now, event, p.Repository.FullName
	})

	actor := p.Pusher.Login
	if actor == "" {
		actor = p.Sender.Login
	}
	switch {
	case p.Repository.FullName == "":
		// e.g. Forgejo's test delivery for a system hook
	case !interesting(event):
	case strings.EqualFold(actor, service):
		rc.Log.Debug("ignored ForgeSync's own change", "node", node, "event", event, "repository", p.Repository.FullName)
	default:
		c := Change{Node: node, Event: event, Action: p.Action, Repository: p.Repository.FullName}
		rc.Log.Info("change reported by webhook", "node", node, "event", event, "action", p.Action,
			"repository", c.Repository, "by", actor)
		// Answer Forgejo now; its delivery worker shouldn't wait for git.
		go rc.Dispatch.Changed(rc.Context, c)
	}
	w.WriteHeader(http.StatusAccepted)
}

func interesting(event string) bool {
	for _, e := range Events {
		if e == event {
			return true
		}
	}
	return false
}

// validSignature checks a hex HMAC-SHA256 of the body, in constant time.
func validSignature(secret string, body []byte, sig string) bool {
	got, err := hex.DecodeString(strings.TrimPrefix(sig, "sha256="))
	if err != nil || len(got) == 0 {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hmac.Equal(got, m.Sum(nil))
}
