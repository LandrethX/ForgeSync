package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"scenegit.org/forgesync/internal/store"
)

type userList struct {
	Total int                `json:"total"`
	Items []store.UserRecord `json:"items"`
}

func userCommand(o *options) *cobra.Command {
	user := &cobra.Command{Use: "user", Short: "SceneID users and their primary site"}

	var query string
	list := &cobra.Command{
		Use:   "list",
		Short: "List SceneID users, their primary site and the nodes they have an account on",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var res userList
			if err := o.get(cmd.Context(), "/api/v1/users?q="+url.QueryEscape(query), &res); err != nil {
				return err
			}
			if o.output == "json" {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "USER\tPRIMARY SITE\tSET BY\tACCOUNTS")
			for _, u := range res.Items {
				var accts []string
				for _, a := range u.Accounts {
					if !a.Present {
						continue
					}
					if a.CreatedByUs {
						accts = append(accts, a.Node+"(copy)")
					} else {
						accts = append(accts, a.Node)
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", u.Login, dash(u.HomeNode), dash(u.HomeSource), strings.Join(accts, " "))
			}
			return tw.Flush()
		},
	}
	list.Flags().StringVarP(&query, "query", "q", "", "only logins containing this text")

	setHome := &cobra.Command{
		Use:   "set-home LOGIN NODE",
		Short: "Change a user's primary site",
		Long: "Change a user's primary site. Needs an administrator (the admin token is one).\n" +
			"By default it's where the user registered. Their repositories follow after the next scan,\n" +
			"except those whose primary an administrator chose.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := o.findUser(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			var res struct {
				Home     string `json:"home_node"`
				Previous string `json:"previous"`
			}
			if err := o.call(cmd.Context(), http.MethodPut, "/api/v1/users/"+id+"/home",
				map[string]string{"node": args[1]}, &res); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: primary site %s (was %s)\n", args[0], res.Home, dash(res.Previous))
			return nil
		},
	}
	user.AddCommand(list, setHome)
	return user
}

func (o *options) findUser(ctx context.Context, login string) (string, error) {
	var res userList
	if err := o.get(ctx, "/api/v1/users?q="+url.QueryEscape(login), &res); err != nil {
		return "", err
	}
	for _, u := range res.Items {
		if strings.EqualFold(u.Login, login) {
			return u.ID, nil
		}
	}
	return "", fmt.Errorf("no SceneID user named %s (has the inventory scan found them yet?)", login)
}
