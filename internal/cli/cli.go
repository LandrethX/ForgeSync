// Package cli implements the forgesync command-line client.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
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
	return root
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(o.server, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
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
