// Package cli implements the forgesync command-line client.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"scenegit.org/forgesync/internal/api"
	"scenegit.org/forgesync/internal/buildinfo"
	"scenegit.org/forgesync/internal/store"
)

type options struct {
	server    string
	token     string
	tokenFile string
	output    string
}

// NewRootCommand builds the command tree, writing results to out.
func NewRootCommand(out io.Writer) *cobra.Command {
	o := &options{}
	root := &cobra.Command{
		Use:           "forgesync",
		Short:         "Command-line client for the ForgeSync controller",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.SetOut(out)
	root.PersistentFlags().StringVar(&o.server, "server", envOr("FORGESYNC_SERVER", "http://127.0.0.1:8090"), "controller URL (env FORGESYNC_SERVER)")
	root.PersistentFlags().StringVar(&o.token, "token", os.Getenv("FORGESYNC_TOKEN"), "admin API token (env FORGESYNC_TOKEN)")
	root.PersistentFlags().StringVar(&o.tokenFile, "token-file", os.Getenv("FORGESYNC_TOKEN_FILE"), "file holding the admin API token (env FORGESYNC_TOKEN_FILE)")
	root.PersistentFlags().StringVarP(&o.output, "output", "o", "table", "output format: table or json")

	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the client and controller versions",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), "client:    ", buildinfo.String())
			var v struct{ Version, Commit string }
			if err := o.get(cmd.Context(), "/api/v1/version", &v); err != nil {
				fmt.Fprintln(cmd.OutOrStdout(), "controller: unavailable:", err)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "controller: %s (%s)\n", v.Version, v.Commit)
			return nil
		},
	})

	node := &cobra.Command{Use: "node", Short: "Forgejo nodes"}
	node.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List nodes and their health",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var nodes []api.Node
			if err := o.get(cmd.Context(), "/api/v1/nodes", &nodes); err != nil {
				return err
			}
			if o.output == "json" {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(nodes)
			}
			return printNodes(cmd.OutOrStdout(), nodes, time.Now())
		},
	})
	root.AddCommand(node)
	root.AddCommand(repoCommand(o))
	root.AddCommand(conflictCommand(o))
	root.AddCommand(historyCommand(o))
	return root
}

func conflictCommand(o *options) *cobra.Command {
	conflict := &cobra.Command{Use: "conflict", Short: "Conflicts between nodes"}
	var state string
	list := &cobra.Command{
		Use:   "list",
		Short: "List conflicts (open ones by default)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var res struct {
				Total  int              `json:"total"`
				Counts map[string]int   `json:"counts"`
				Items  []store.Conflict `json:"items"`
			}
			if err := o.call(cmd.Context(), http.MethodGet, "/api/v1/conflicts?limit=500&state="+url.QueryEscape(state), nil, &res); err != nil {
				return err
			}
			if o.output == "json" {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tREPOSITORY\tKIND\tSTATE\tDETECTED\tNODES")
			for _, c := range res.Items {
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", c.ID, c.FullName, c.Kind, c.State,
					c.DetectedAt.Local().Format("2006-01-02 15:04"), conflictNodes(c))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%d open, %d cleared\n", res.Counts["open"], res.Counts["cleared"])
			return nil
		},
	}
	list.Flags().StringVar(&state, "state", "open", "open, cleared or all")
	conflict.AddCommand(list)
	return conflict
}

// conflictNodes summarises what each node has, e.g. "se:a1b2c3d dk:9f8e7d6".
func conflictNodes(c store.Conflict) string {
	key, short := "heads", 7
	if c.Kind == "default_branch_mismatch" {
		key, short = "branches", 0
	}
	m, _ := c.Details[key].(map[string]any)
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	var parts []string
	for _, n := range names {
		v, _ := m[n].(string)
		if short > 0 && len(v) > short {
			v = v[:short]
		}
		parts = append(parts, n+":"+v)
	}
	return strings.Join(parts, " ")
}

type repoList struct {
	Total  int              `json:"total"`
	Counts map[string]int   `json:"counts"`
	Items  []api.Repository `json:"items"`
}

func repoCommand(o *options) *cobra.Command {
	repo := &cobra.Command{Use: "repo", Short: "Repositories across nodes"}

	var status, query string
	var limit int
	list := &cobra.Command{
		Use:   "list",
		Short: "List repositories and whether each node has the same default branch commit",
		RunE: func(cmd *cobra.Command, _ []string) error {
			v := url.Values{"limit": {strconv.Itoa(limit)}}
			if status != "" {
				v.Set("status", status)
			}
			if query != "" {
				v.Set("q", query)
			}
			var res repoList
			if err := o.call(cmd.Context(), http.MethodGet, "/api/v1/repositories?"+v.Encode(), nil, &res); err != nil {
				return err
			}
			if o.output == "json" {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "REPOSITORY\tSTATUS\tPRIMARY\tNODES")
			for _, r := range res.Items {
				var nodes []string
				for _, n := range r.Nodes {
					nodes = append(nodes, n.Node+":"+string(n.Presence))
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.FullName, r.Status, dash(r.PrimaryNode), strings.Join(nodes, " "))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if res.Total > len(res.Items) {
				fmt.Fprintf(cmd.OutOrStdout(), "(%d of %d shown; use --limit)\n", len(res.Items), res.Total)
			}
			return nil
		},
	}
	list.Flags().StringVar(&status, "status", "", "only this status: same, differs, missing or unknown")
	list.Flags().StringVarP(&query, "query", "q", "", "only names containing this text")
	list.Flags().IntVar(&limit, "limit", 500, "maximum number of repositories to show")

	setPrimary := &cobra.Command{
		Use:   "set-primary OWNER/NAME NODE",
		Short: "Record which node is a repository's primary (use - to clear)",
		Long: "Record which node is a repository's primary. Needs an administrator (the admin token is one).\n" +
			"ForgeSync doesn't replicate yet, so this only records the designation.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := o.findRepository(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			node := args[1]
			if node == "-" {
				node = ""
			}
			var res struct {
				Primary  string `json:"primary_node"`
				Previous string `json:"previous"`
			}
			if err := o.call(cmd.Context(), http.MethodPut, "/api/v1/repositories/"+id+"/primary",
				map[string]string{"node": node}, &res); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: primary %s (was %s)\n", args[0], dash(res.Primary), dash(res.Previous))
			return nil
		},
	}
	repo.AddCommand(list, setPrimary)
	return repo
}

// findRepository looks up a repository's id by its exact owner/name.
func (o *options) findRepository(ctx context.Context, fullName string) (string, error) {
	var res repoList
	if err := o.call(ctx, http.MethodGet, "/api/v1/repositories?limit=500&q="+url.QueryEscape(fullName), nil, &res); err != nil {
		return "", err
	}
	for _, r := range res.Items {
		if strings.EqualFold(r.FullName, fullName) {
			return r.ID, nil
		}
	}
	return "", fmt.Errorf("no repository named %s (has the inventory scan found it yet?)", fullName)
}

func printNodes(w io.Writer, nodes []api.Node, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSITE\tSTATE\tVERSION\tLAST SEEN\tURL")
	for _, n := range nodes {
		seen := "never"
		if n.LastSeen != nil {
			seen = now.Sub(*n.LastSeen).Round(time.Second).String() + " ago"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", n.Name, n.Site, n.State, dash(n.Version), seen, n.URL)
	}
	return tw.Flush()
}

func (o *options) get(ctx context.Context, path string, out any) error {
	return o.call(ctx, http.MethodGet, path, nil, out)
}

func (o *options) call(ctx context.Context, method, path string, in, out any) error {
	resp, err := o.send(ctx, method, path, in, 15*time.Second)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

// download streams a GET response body to w, for exports.
func (o *options) download(ctx context.Context, path string, w io.Writer) (int64, error) {
	resp, err := o.send(ctx, http.MethodGet, path, nil, 10*time.Minute)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return io.Copy(w, resp.Body)
}

// send makes an authenticated request and returns the response if it is
// 200 OK; otherwise an error with the server's message.
func (o *options) send(ctx context.Context, method, path string, in any, timeout time.Duration) (*http.Response, error) {
	token := o.token
	if token == "" && o.tokenFile != "" {
		b, err := os.ReadFile(o.tokenFile)
		if err != nil {
			return nil, err
		}
		token = strings.TrimSpace(string(b))
	}
	if token == "" {
		return nil, errors.New("no admin token: use --token, --token-file, FORGESYNC_TOKEN or FORGESYNC_TOKEN_FILE")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(o.server, "/")+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		var e struct{ Message string }
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return nil, fmt.Errorf("%s: %s %s", path, resp.Status, e.Message)
	}
	return resp, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
