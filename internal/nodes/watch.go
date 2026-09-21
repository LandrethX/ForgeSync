package nodes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"scenegit.org/forgesync/internal/config"
	"scenegit.org/forgesync/internal/store"
)

// A controller reads the node set once, at startup, and wires seven
// things from it: the health monitor, the scanner, the conflict
// comparers, the replication engine, the issue syncer, the webhook
// installer and the API's own list. Adding a node therefore has to reach
// all seven.
//
// Rebuilding them in place would mean making every one of them accept a
// changing set, which is a large change to a part of ForgeSync that is
// working. Restarting is the other way, and it is unusually cheap here by
// construction: every loop is idempotent and picked up again, the lease
// is given up on a clean stop so another controller takes over in about a
// renewal, and systemd brings the process back in a second or two. So
// that is what this does, and it says so rather than pretending the
// change was applied in flight.
//
// What it must not do is restart when nothing has changed, so it compares
// a fingerprint of what actually matters, and only what a controller
// would wire differently.

// Fingerprint is what a controller would have to be rebuilt to follow.
// The token is in it, because a node whose token was replaced needs the
// new one; its bytes are hashed, never kept.
func Fingerprint(ns []Node) string {
	lines := make([]string, 0, len(ns))
	for _, n := range ns {
		lines = append(lines, fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d\x00%x",
			n.Name, n.URL, n.Site, n.ServiceUser, n.SceneIDSourceID, sha256.Sum256([]byte(n.Token))))
	}
	sort.Strings(lines)
	sum := sha256.New()
	for _, l := range lines {
		sum.Write([]byte(l))
		sum.Write([]byte("\n"))
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// keyOpener is the part of *secret.Key this needs, as an interface so the
// tests do not have to make a real one.
type keyOpener interface {
	Open(sealed []byte) (string, error)
}

// Watch returns when the node set has changed from the fingerprint given,
// or when ctx ends. It never returns an error: a database it cannot read
// is somebody else's problem to report, and the worst thing this could do
// is restart a controller because a query failed.
func Watch(ctx context.Context, db Store, key keyOpener, configured []config.Node, was string, every time.Duration, log *slog.Logger) {
	if every <= 0 {
		every = 30 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		stored, err := db.Nodes(ctx)
		if err != nil {
			continue // it will be reported by whatever else is failing
		}
		now, err := fingerprintStored(stored, configured, key)
		if err != nil {
			// A node whose token this controller cannot open is a real
			// problem, but restarting would not fix it and would loop.
			if log != nil {
				log.Warn("a node in the database cannot be read by this controller", "error", err)
			}
			continue
		}
		if now != was {
			if log != nil {
				log.Info("the installation's nodes have changed; restarting to pick them up",
					"nodes", len(stored))
			}
			return
		}
	}
}

// fingerprintStored builds the node set the same way Resolve does, and
// fingerprints that. Building it any other way is how this restarted a
// controller every fifteen seconds: a node taken from the config file
// keeps its token there, and leaving that token out here made the two
// fingerprints differ for ever.
func fingerprintStored(stored []store.NodeRecord, configured []config.Node, key keyOpener) (string, error) {
	byName := map[string]config.Node{}
	for _, c := range configured {
		byName[c.Name] = c
	}
	ns := make([]Node, 0, len(stored))
	for _, rec := range stored {
		n, err := fromRecord(rec, byName, key)
		if err != nil {
			return "", err
		}
		ns = append(ns, n)
	}
	return Fingerprint(ns), nil
}
