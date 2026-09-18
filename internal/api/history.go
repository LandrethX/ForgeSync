package api

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"scenegit.org/forgesync/internal/store"
)

// maxExport bounds one export; narrow the filters for more.
const maxExport = 50000

var categoryName = regexp.MustCompile(`^[a-z_]{1,32}$`)

// historyFilter reads ?category=a,b ?actor= ?q= ?from= ?to= (RFC 3339)
// ?limit= ?cursor= into a filter.
func historyFilter(r *http.Request) (store.EventFilter, error) {
	q := r.URL.Query()
	var f store.EventFilter
	for _, c := range strings.Split(q.Get("category"), ",") {
		if c = strings.TrimSpace(c); c == "" {
			continue
		}
		if !categoryName.MatchString(c) {
			return f, fmt.Errorf("bad category %q", c)
		}
		f.Categories = append(f.Categories, c)
	}
	f.Actor = q.Get("actor")
	f.Query = q.Get("q")
	if len(f.Query) > 200 {
		return f, errors.New("q can be at most 200 characters")
	}
	for name, dst := range map[string]*time.Time{"from": &f.From, "to": &f.To} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return f, fmt.Errorf("%s must be an RFC 3339 time, e.g. 2026-09-18T10:00:00Z", name)
			}
			*dst = t
		}
	}
	var err error
	if f.Limit, err = queryInt(r, "limit", 100, 1, 500); err != nil {
		return f, err
	}
	f.Cursor = q.Get("cursor")
	return f, nil
}

type historyPage struct {
	Items      []store.Event `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

// listHistory returns the combined audit log and node health history,
// newest first. Operators and up: it shows who signed in from where.
func (s *Server) listHistory(w http.ResponseWriter, r *http.Request) {
	f, err := historyFilter(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	items, next, err := s.DB.History(r.Context(), f)
	if errors.Is(err, store.ErrBadCursor) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid cursor"})
		return
	}
	if err != nil {
		s.serverError(w, "list history", err)
		return
	}
	if items == nil {
		items = []store.Event{}
	}
	writeJSON(w, http.StatusOK, historyPage{Items: items, NextCursor: next})
}

func (s *Server) historyActors(w http.ResponseWriter, r *http.Request) {
	actors, err := s.DB.HistoryActors(r.Context())
	if err != nil {
		s.serverError(w, "list actors", err)
		return
	}
	writeJSON(w, http.StatusOK, actors)
}

// exportHistory streams matching entries as CSV or JSON (?format=csv|json).
// Exports are audited, since they take the audit log out of ForgeSync.
func (s *Server) exportHistory(w http.ResponseWriter, r *http.Request) {
	f, err := historyFilter(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "csv"
	}
	if format != "csv" && format != "json" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "format must be csv or json"})
		return
	}
	// Streaming a large export can take longer than the server's write timeout.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Minute))

	s.audit(r.Context(), identity(r).Actor(), "history.exported", "", map[string]any{
		"format": format, "categories": f.Categories, "actor": f.Actor, "q": f.Query,
		"from": timeOrEmpty(f.From), "to": timeOrEmpty(f.To),
	})
	name := "forgesync-history-" + time.Now().UTC().Format("20060102-150405") + "." + format
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)

	if format == "json" {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		fmt.Fprint(w, "[")
		first := true
		err = s.DB.HistoryEach(r.Context(), f, maxExport, func(e store.Event) error {
			if !first {
				fmt.Fprint(w, ",")
			}
			first = false
			return enc.Encode(e)
		})
		fmt.Fprint(w, "]\n")
	} else {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		cw := csv.NewWriter(w)
		cw.Write([]string{"time", "category", "actor", "action", "target", "details"})
		err = s.DB.HistoryEach(r.Context(), f, maxExport, func(e store.Event) error {
			details, _ := json.Marshal(e.Details)
			return cw.Write([]string{e.At.UTC().Format(time.RFC3339Nano), e.Category,
				csvSafe(e.Actor), csvSafe(e.Action), csvSafe(e.Target), csvSafe(string(details))})
		})
		cw.Flush()
	}
	if err != nil {
		// Headers are already sent; all we can do is log and cut the stream short.
		s.Log.Error("history export failed", "error", err)
	}
}

// csvSafe stops spreadsheet programs from treating a cell as a formula.
func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

func timeOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
