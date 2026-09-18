package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Event is one entry in the combined history: an audit log entry, or a
// node health state change (action "node.state_changed").
type Event struct {
	ID       string         `json:"id"` // "a<n>" for audit entries, "n<n>" for node changes
	At       time.Time      `json:"at"`
	Category string         `json:"category"` // first part of the action: session, repo, conflict, inventory, node
	Actor    string         `json:"actor"`
	Action   string         `json:"action"`
	Target   string         `json:"target"`
	Details  map[string]any `json:"details"`
}

// EventFilter selects history entries. Zero values don't filter.
type EventFilter struct {
	Categories []string
	Actor      string
	Query      string // case-insensitive text anywhere in actor, action, target or details
	From, To   time.Time
	Limit      int
	Cursor     string // from a previous page's NextCursor
}

// ErrBadCursor is returned for a cursor that wasn't produced by History.
var ErrBadCursor = errors.New("invalid cursor")

// history is the combined history as one relation.
const history = `(
	SELECT 'a' AS src, id, at, split_part(action, '.', 1) AS category, actor, action, target, details
	FROM audit_log
	UNION ALL
	SELECT 'n', id, at, 'node', 'forgesync', 'node.state_changed', node,
		jsonb_build_object('from', from_state, 'to', to_state, 'error', error)
	FROM node_state_transitions
) h`

// History returns matching entries, newest first, and a cursor for the next
// page ("" when there is none).
func (s *Store) History(ctx context.Context, f EventFilter) ([]Event, string, error) {
	where, args, err := historyWhere(f)
	if err != nil {
		return nil, "", err
	}
	args = append(args, f.Limit+1)
	rows, err := s.pool.Query(ctx, `
		SELECT src, id, at, category, actor, action, target, details FROM `+history+`
		WHERE `+where+`
		ORDER BY at DESC, src DESC, id DESC
		LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, "", err
	}
	type row struct {
		src string
		id  int64
		ev  Event
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.src, &x.id, &x.ev.At, &x.ev.Category, &x.ev.Actor, &x.ev.Action, &x.ev.Target, &x.ev.Details)
		x.ev.ID = x.src + strconv.FormatInt(x.id, 10)
		return x, err
	})
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(list) > f.Limit {
		last := list[f.Limit-1]
		next = encodeCursor(last.ev.At, last.src, last.id)
		list = list[:f.Limit]
	}
	out := make([]Event, len(list))
	for i, x := range list {
		out[i] = x.ev
	}
	return out, next, nil
}

// HistoryEach calls fn for every matching entry, newest first, up to max.
// It's for exports, which shouldn't hold everything in memory.
func (s *Store) HistoryEach(ctx context.Context, f EventFilter, max int, fn func(Event) error) error {
	f.Cursor = ""
	where, args, err := historyWhere(f)
	if err != nil {
		return err
	}
	args = append(args, max)
	rows, err := s.pool.Query(ctx, `
		SELECT src, id, at, category, actor, action, target, details FROM `+history+`
		WHERE `+where+`
		ORDER BY at DESC, src DESC, id DESC
		LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var src string
		var id int64
		var ev Event
		if err := rows.Scan(&src, &id, &ev.At, &ev.Category, &ev.Actor, &ev.Action, &ev.Target, &ev.Details); err != nil {
			return err
		}
		ev.ID = src + strconv.FormatInt(id, 10)
		if err := fn(ev); err != nil {
			return err
		}
	}
	return rows.Err()
}

func historyWhere(f EventFilter) (string, []any, error) {
	conds := []string{"true"}
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if len(f.Categories) > 0 {
		conds = append(conds, "category = ANY("+arg(f.Categories)+"::text[])")
	}
	if f.Actor != "" {
		conds = append(conds, "actor = "+arg(f.Actor))
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		p := arg("%" + escapeLike(q) + "%")
		conds = append(conds, fmt.Sprintf("(actor ILIKE %[1]s OR action ILIKE %[1]s OR target ILIKE %[1]s OR details::text ILIKE %[1]s)", p))
	}
	if !f.From.IsZero() {
		conds = append(conds, "at >= "+arg(f.From))
	}
	if !f.To.IsZero() {
		conds = append(conds, "at < "+arg(f.To))
	}
	if f.Cursor != "" {
		at, src, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return "", nil, err
		}
		conds = append(conds, fmt.Sprintf("(at, src, id) < (%s, %s, %s)", arg(at), arg(src), arg(id)))
	}
	return strings.Join(conds, " AND "), args, nil
}

// HistoryActors lists everyone who appears in the history, for filters.
func (s *Store) HistoryActors(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT actor FROM (SELECT DISTINCT actor FROM audit_log UNION SELECT 'forgesync') a
		ORDER BY actor LIMIT 500`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func encodeCursor(at time.Time, src string, id int64) string {
	raw := at.UTC().Format(time.RFC3339Nano) + "|" + src + "|" + strconv.FormatInt(id, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(c string) (time.Time, string, int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, "", 0, ErrBadCursor
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 || (parts[1] != "a" && parts[1] != "n") {
		return time.Time{}, "", 0, ErrBadCursor
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", 0, ErrBadCursor
	}
	id, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return time.Time{}, "", 0, ErrBadCursor
	}
	return at, parts[1], id, nil
}
