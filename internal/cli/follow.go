package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"time"

	"scenegit.org/forgesync/internal/store"
)

// lookback is how far before the newest entry seen each poll asks again.
// Node health changes carry the time of the check, which can be a little
// before they are written; asking again for a window and skipping entries
// already shown means none is missed.
const lookback = 2 * time.Minute

// maxPerPoll bounds one poll; a burst larger than this within one interval
// is shown up to the limit, with a note.
const maxPerPoll = 5000

type follower struct {
	o        *options
	params   url.Values // filters, without a time window of their own
	from     time.Time  // user's --since/--from, zero if none
	jsonOut  bool
	details  bool
	out, err io.Writer
	interval time.Duration

	seen   map[string]time.Time // entry id -> time, for entries within the lookback window
	newest time.Time
}

// run prints the newest `initial` entries, then new ones as they arrive,
// until ctx ends. Transient errors are retried; losing access stops it.
func (f *follower) run(ctx context.Context, initial int) error {
	f.seen = map[string]time.Time{}
	events, _, err := fetchHistory(ctx, f.o, f.window(time.Time{}), initial)
	if err != nil {
		return err
	}
	if !f.jsonOut {
		header := followHeader
		if f.details {
			header += "  DETAILS"
		}
		fmt.Fprintln(f.out, header)
	}
	f.emit(events)
	// Entries in the look-back window that didn't fit in the first screen
	// already existed; mark them seen so the first poll doesn't print them
	// as new, out of order.
	if !f.newest.IsZero() {
		existing, _, err := fetchHistory(ctx, f.o, f.window(f.newest.Add(-lookback)), maxPerPoll)
		if err != nil {
			return err
		}
		for _, e := range existing {
			f.seen[e.ID] = e.At
		}
	}

	failures := 0
	for {
		wait := f.interval
		if failures > 0 {
			// Back off on repeated failures, up to 30 seconds.
			wait = min(f.interval<<min(failures, 5), 30*time.Second)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}

		since := f.newest.Add(-lookback)
		events, more, err := fetchHistory(ctx, f.o, f.window(since), maxPerPoll)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			var he *httpError
			if errors.As(err, &he) && (he.Code == http.StatusUnauthorized || he.Code == http.StatusForbidden) {
				return err
			}
			failures++
			fmt.Fprintf(f.err, "forgesync: %v (retrying)\n", err)
			continue
		}
		if failures > 0 {
			fmt.Fprintln(f.err, "forgesync: connection restored")
			failures = 0
		}
		f.emit(events)
		if more {
			fmt.Fprintf(f.err, "forgesync: more than %d entries arrived at once; some weren't shown (narrow the filters)\n", maxPerPoll)
		}
		f.prune(since)
	}
}

// window is the filters plus a start time: the later of the user's own
// start and since.
func (f *follower) window(since time.Time) url.Values {
	p := url.Values{}
	for k, v := range f.params {
		p[k] = v
	}
	from := f.from
	if since.After(from) {
		from = since
	}
	if !from.IsZero() {
		p.Set("from", from.UTC().Format(time.RFC3339Nano))
	}
	return p
}

// emit prints the entries not shown yet, oldest first.
func (f *follower) emit(events []store.Event) {
	var fresh []store.Event
	for _, e := range events {
		if _, ok := f.seen[e.ID]; ok {
			continue
		}
		f.seen[e.ID] = e.At
		fresh = append(fresh, e)
		if e.At.After(f.newest) {
			f.newest = e.At
		}
	}
	sort.SliceStable(fresh, func(i, j int) bool {
		if !fresh[i].At.Equal(fresh[j].At) {
			return fresh[i].At.Before(fresh[j].At)
		}
		return fresh[i].ID < fresh[j].ID
	})
	enc := json.NewEncoder(f.out)
	for _, e := range fresh {
		if f.jsonOut {
			enc.Encode(e) // one JSON object per line
			continue
		}
		row := fmt.Sprintf(followRow, e.At.Local().Format("2006-01-02 15:04:05"), e.Category, e.Actor, e.Action, dash(e.Target))
		if f.details {
			b, _ := json.Marshal(e.Details)
			row += "  " + string(b)
		}
		fmt.Fprintln(f.out, oneLine(row))
	}
}

// prune forgets entries that can no longer come back in a poll.
func (f *follower) prune(since time.Time) {
	for id, at := range f.seen {
		if at.Before(since) {
			delete(f.seen, id)
		}
	}
}

// Rows are printed as they arrive, so columns have fixed widths instead of
// being aligned over the whole output.
const (
	followRow    = "%-19s  %-10s  %-24s  %-26s  %s"
	followHeader = "TIME                 CATEGORY    ACTOR                     ACTION                      TARGET"
)
