// Package nodes settles which Forgejo nodes an installation has.
//
// They used to come from the config file alone, which meant adding one was
// a file edit on every controller followed by a restart of each. They live
// in the shared database now, so adding one is a single write every
// controller sees. The config file still works and still wins nothing: a
// node listed there that the database hasn't got is taken into the
// database the first time a controller sees it, which is what carries an
// existing installation across without anybody doing anything.
//
// A node's token is the awkward part. In the database it is sealed with a
// key the controllers hold and the database never sees (internal/secret).
// A node that came from the config file before there was a key has no
// sealed token, and keeps taking its token from the file; that is the
// state every installation starts in, and it is not a broken one.
package nodes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"scenegit.org/forgesync/internal/config"
	"scenegit.org/forgesync/internal/secret"
	"scenegit.org/forgesync/internal/store"
)

// Node is one Forgejo server, with everything needed to reach it.
type Node struct {
	Name            string
	URL             string
	Site            string
	ServiceUser     string
	SceneIDSourceID int64
	Token           string
	// FromDatabase says the token came out of the database rather than a
	// file, which is what an administrator changes by adding the node in
	// the UI.
	FromDatabase bool
}

// Store is the part of the database this needs.
type Store interface {
	Nodes(ctx context.Context) ([]store.NodeRecord, error)
	SaveNode(ctx context.Context, n store.NodeRecord) error
}

// ErrNoToken says a node in the database has no token anywhere: not sealed
// in its row, and not in the config file either. It names the node,
// because the answer is to give that one a token.
var ErrNoToken = errors.New("no token for this node")

// Resolve returns the nodes this controller should work with, importing
// any the config file lists that the database hasn't got yet.
//
// key may be nil, which is an installation that has not set one up: nodes
// still come from the config file and nothing is sealed. A node whose
// token is sealed cannot be used without the key, and that is reported
// rather than skipped, because quietly running with fewer nodes than the
// installation has is how replication stops without anybody noticing.
func Resolve(ctx context.Context, db Store, key *secret.Key, configured []config.Node, log *slog.Logger) ([]Node, error) {
	stored, err := db.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]config.Node{}
	for _, c := range configured {
		byName[c.Name] = c
	}
	known := map[string]bool{}

	var out []Node
	for _, rec := range stored {
		known[rec.Name] = true
		n := Node{Name: rec.Name, URL: rec.URL, Site: rec.Site,
			ServiceUser: rec.ServiceUser, SceneIDSourceID: rec.SceneIDSourceID}
		switch {
		case len(rec.SealedToken) > 0:
			if key == nil {
				return nil, fmt.Errorf("node %q: its token is sealed but node_key_file is not set", rec.Name)
			}
			tok, err := key.Open(rec.SealedToken)
			if err != nil {
				return nil, fmt.Errorf("node %q: %w", rec.Name, err)
			}
			n.Token, n.FromDatabase = tok, true
		default:
			c, ok := byName[rec.Name]
			if !ok || c.Token == "" {
				return nil, fmt.Errorf("node %q: %w; it is in the database without a sealed token and not in the config file", rec.Name, ErrNoToken)
			}
			n.Token = c.Token
			// The config may have moved the node or corrected its details.
			n.URL, n.Site, n.ServiceUser, n.SceneIDSourceID = c.URL, c.Site, c.ServiceUser, c.SceneIDSourceID
		}
		out = append(out, n)
	}

	// Anything in the config file the database hasn't seen. On a first
	// start that is every node, which is how an existing installation
	// moves across without being touched.
	for _, c := range configured {
		if known[c.Name] {
			continue
		}
		rec := store.NodeRecord{Name: c.Name, URL: c.URL, Site: c.Site,
			ServiceUser: c.ServiceUser, SceneIDSourceID: c.SceneIDSourceID, Source: "config"}
		if err := db.SaveNode(ctx, rec); err != nil {
			return nil, err
		}
		if log != nil {
			log.Info("took a node from the config file into the database", "node", c.Name)
		}
		out = append(out, Node{Name: c.Name, URL: c.URL, Site: c.Site, ServiceUser: c.ServiceUser,
			SceneIDSourceID: c.SceneIDSourceID, Token: c.Token})
	}
	return out, nil
}
