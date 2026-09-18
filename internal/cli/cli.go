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
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"scenegit.org/forgesync/internal/api"
	"scenegit.org/forgesync/internal/buildinfo"
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
	return root
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
	token := o.token
	if token == "" && o.tokenFile != "" {
		b, err := os.ReadFile(o.tokenFile)
		if err != nil {
			return err
		}
		token = strings.TrimSpace(string(b))
	}
	if token == "" {
		return errors.New("no admin token: use --token, --token-file, FORGESYNC_TOKEN or FORGESYNC_TOKEN_FILE")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(o.server, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct{ Message string }
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%s: %s %s", path, resp.Status, e.Message)
	}
	return json.NewDecoder(resp.Body).Decode(out)
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
