package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"scenegit.org/forgesync/internal/store"
)

type historyFlags struct {
	categories []string
	actor      string
	query      string
	since      string
	from, to   string
	limit      int
	details    bool
	export     string
	file       string
	follow     bool
	interval   time.Duration
}

func historyCommand(o *options) *cobra.Command {
	f := &historyFlags{}
	cmd := &cobra.Command{
		Use:   "history",
		Short: "Show or export the event and audit history",
		Long: "Show the event and audit history, newest first: sign-ins, administrative actions,\n" +
			"conflicts, scans, exports and node health changes. Needs the operator role.\n\n" +
			"Categories: session, node, repo, conflict, inventory, history.",
		Example: "  forgesync history --since 24h\n" +
			"  forgesync history --category node -q unreachable --since 7d\n" +
			"  forgesync history --actor sceneid:alice --from 2026-09-01 --to 2026-09-30\n" +
			"  forgesync history --category session --since 30d --export csv --file sign-ins.csv\n" +
			"  forgesync history -f -c node,conflict\n" +
			"  forgesync history -f -o json | jq -r .action",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			params, err := f.params(time.Now())
			if err != nil {
				return err
			}
			if f.export != "" {
				return f.runExport(cmd, o, params)
			}
			if f.follow {
				return f.runFollow(cmd, o, params)
			}
			events, more, err := fetchHistory(cmd.Context(), o, params, f.limit)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.output == "json" {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(events)
			}
			if err := printHistory(out, events, f.details); err != nil {
				return err
			}
			if more {
				fmt.Fprintf(cmd.ErrOrStderr(), "(showing the newest %d; use --limit or narrow the filters for more)\n", len(events))
			}
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringSliceVarP(&f.categories, "category", "c", nil, "only these categories (repeat or comma-separate)")
	fl.StringVar(&f.actor, "actor", "", "only entries by this actor, e.g. sceneid:alice, token, forgesync")
	fl.StringVarP(&f.query, "query", "q", "", "text to find in actor, action, target or details")
	fl.StringVar(&f.since, "since", "", "only the last period, e.g. 90m, 24h, 7d")
	fl.StringVar(&f.from, "from", "", "start: a date (local midnight) or an RFC 3339 time")
	fl.StringVar(&f.to, "to", "", "end: a date (whole day included) or an RFC 3339 time")
	fl.IntVar(&f.limit, "limit", 50, "maximum number of entries to show")
	fl.BoolVar(&f.details, "details", false, "add a column with each entry's details")
	fl.StringVar(&f.export, "export", "", "export everything that matches as csv or json (ignores --limit, max 50,000 rows); the export is audited")
	fl.StringVar(&f.file, "file", "", "with --export: write to this file instead of standard output")
	fl.BoolVarP(&f.follow, "follow", "f", false, "keep running and print new entries as they happen (oldest first; Ctrl-C to stop)")
	fl.DurationVar(&f.interval, "interval", 2*time.Second, "with --follow: how often to check for new entries")
	cmd.MarkFlagsMutuallyExclusive("since", "from")
	cmd.MarkFlagsMutuallyExclusive("follow", "export")
	cmd.MarkFlagsMutuallyExclusive("follow", "to")
	return cmd
}

// params turns the flags into the API's query parameters.
func (f *historyFlags) params(now time.Time) (url.Values, error) {
	v := url.Values{}
	if len(f.categories) > 0 {
		v.Set("category", strings.Join(f.categories, ","))
	}
	if f.actor != "" {
		v.Set("actor", f.actor)
	}
	if f.query != "" {
		v.Set("q", f.query)
	}
	if f.since != "" {
		d, err := parseSince(f.since)
		if err != nil {
			return nil, err
		}
		v.Set("from", now.Add(-d).UTC().Format(time.RFC3339))
	}
	if f.from != "" {
		t, err := parseWhen(f.from, false)
		if err != nil {
			return nil, fmt.Errorf("--from: %w", err)
		}
		v.Set("from", t.UTC().Format(time.RFC3339))
	}
	if f.to != "" {
		t, err := parseWhen(f.to, true)
		if err != nil {
			return nil, fmt.Errorf("--to: %w", err)
		}
		v.Set("to", t.UTC().Format(time.RFC3339))
	}
	if f.limit < 1 {
		return nil, errors.New("--limit must be at least 1")
	}
	if f.export != "" && f.export != "csv" && f.export != "json" {
		return nil, errors.New("--export must be csv or json")
	}
	if f.file != "" && f.export == "" {
		return nil, errors.New("--file needs --export")
	}
	if f.follow && f.interval < time.Second {
		return nil, errors.New("--interval must be at least 1s")
	}
	return v, nil
}

// parseSince accepts Go durations plus days: 90m, 24h, 7d, 1d12h.
func parseSince(s string) (time.Duration, error) {
	var days time.Duration
	if i := strings.IndexByte(s, 'd'); i > 0 {
		n, err := strconv.Atoi(s[:i])
		if err != nil {
			return 0, fmt.Errorf("--since %q: use e.g. 90m, 24h or 7d", s)
		}
		days, s = time.Duration(n)*24*time.Hour, s[i+1:]
	}
	var rest time.Duration
	if s != "" {
		var err error
		if rest, err = time.ParseDuration(s); err != nil {
			return 0, fmt.Errorf("--since: use e.g. 90m, 24h or 7d")
		}
	}
	if d := days + rest; d > 0 {
		return d, nil
	}
	return 0, errors.New("--since must be positive")
}

// parseWhen reads a local date (YYYY-MM-DD) or an RFC 3339 time. For an end
// date, the whole day is included.
func parseWhen(s string, end bool) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	d, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is neither a date (2026-09-18) nor an RFC 3339 time", s)
	}
	if end {
		d = d.AddDate(0, 0, 1)
	}
	return d, nil
}

// fetchHistory pages through the history until limit entries, and reports
// whether more matched.
func fetchHistory(ctx context.Context, o *options, params url.Values, limit int) ([]store.Event, bool, error) {
	events := []store.Event{}
	cursor := ""
	for len(events) < limit {
		p := url.Values{}
		for k, v := range params {
			p[k] = v
		}
		p.Set("limit", strconv.Itoa(min(limit-len(events), 500)))
		if cursor != "" {
			p.Set("cursor", cursor)
		}
		var page struct {
			Items      []store.Event `json:"items"`
			NextCursor string        `json:"next_cursor"`
		}
		if err := o.call(ctx, http.MethodGet, "/api/v1/history?"+p.Encode(), nil, &page); err != nil {
			return nil, false, err
		}
		events = append(events, page.Items...)
		if page.NextCursor == "" {
			return events, false, nil
		}
		cursor = page.NextCursor
	}
	return events, true, nil
}

func printHistory(w io.Writer, events []store.Event, details bool) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	header := "TIME\tCATEGORY\tACTOR\tACTION\tTARGET"
	if details {
		header += "\tDETAILS"
	}
	fmt.Fprintln(tw, header)
	for _, e := range events {
		line := fmt.Sprintf("%s\t%s\t%s\t%s\t%s", e.At.Local().Format("2006-01-02 15:04:05"),
			e.Category, e.Actor, e.Action, dash(e.Target))
		if details {
			b, _ := json.Marshal(e.Details)
			line += "\t" + string(b)
		}
		fmt.Fprintln(tw, oneLine(line))
	}
	return tw.Flush()
}

// oneLine keeps each entry on one row even if a value contains newlines.
func oneLine(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
}

func (f *historyFlags) runExport(cmd *cobra.Command, o *options, params url.Values) error {
	params.Set("format", f.export)
	w := cmd.OutOrStdout()
	if f.file != "" {
		file, err := os.OpenFile(f.file, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		defer file.Close()
		w = file
	}
	n, err := o.download(cmd.Context(), "/api/v1/history/export?"+params.Encode(), w)
	if err != nil {
		return err
	}
	if f.file != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "wrote %d bytes to %s\n", n, f.file)
	}
	return nil
}

func (f *historyFlags) runFollow(cmd *cobra.Command, o *options, params url.Values) error {
	// The time window moves while following; keep only the user's start.
	var from time.Time
	if v := params.Get("from"); v != "" {
		from, _ = time.Parse(time.RFC3339, v)
		params.Del("from")
	}
	initial := f.limit
	if !cmd.Flags().Changed("limit") {
		initial = 10 // like tail -f
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fw := &follower{
		o: o, params: params, from: from, jsonOut: o.output == "json", details: f.details,
		out: cmd.OutOrStdout(), err: cmd.ErrOrStderr(), interval: f.interval,
	}
	return fw.run(ctx, initial)
}
