package api

import (
	"net/http"

	"scenegit.org/forgesync/internal/store"
)

// replicationSources says what each node has sent: the repositories it is
// the primary of, where their copies went and when they last did.
//
// A node's page can show what it holds, and a repository's page can show
// where it went, but neither answers "what has this node sent, and is any
// of it stuck" without reading every repository. The nodes page asks this
// instead.
func (s *Server) replicationSources(w http.ResponseWriter, r *http.Request) {
	out := replicationSources{Enabled: s.Replication != nil, Pairs: []store.SourcePair{}}
	if !out.Enabled {
		writeJSON(w, http.StatusOK, out)
		return
	}
	pairs, err := s.DB.SourcePairs(r.Context())
	if err != nil {
		s.serverError(w, "list replication sources", err)
		return
	}
	if pairs != nil {
		out.Pairs = pairs
	}
	writeJSON(w, http.StatusOK, out)
}

type replicationSources struct {
	// Enabled is false when this controller doesn't replicate at all, so
	// the page says that rather than showing an empty table.
	Enabled bool               `json:"enabled"`
	Pairs   []store.SourcePair `json:"pairs"`
}
