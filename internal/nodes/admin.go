package nodes

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/secret"
	"scenegit.org/forgesync/internal/store"
)

// Adding a node used to be a block in a file on every controller and a
// restart of each. It is a write to the shared database now, which is
// what lets it be done from the admin UI and be true everywhere at once.
//
// Nothing here is done on the Forgejo node's behalf. ForgeSync asks it
// what it is, says what it found, and stores the answer; setting up the
// node is the administrator's job, because the parts that matter most
// (the app.ini keys) cannot be reached through the API at all and need
// the node restarted. What this does instead is refuse to save a node it
// could not reach or could not use, so the failure happens while somebody
// is looking at it rather than on the next replication round.

// nodeName is what a node may be called: the name goes in URLs, in the
// history and in conflict details, so it stays plain.
var nodeName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$`)

// NewNode is a node somebody is asking ForgeSync to take on.
type NewNode struct {
	Name            string `json:"name"`
	URL             string `json:"url"`
	Site            string `json:"site"`
	ServiceUser     string `json:"service_user"`
	SceneIDSourceID int64  `json:"sceneid_source_id"`
	// Token is the API token of the service account on that node. It is
	// never returned by anything here.
	Token string `json:"token"`
}

// Finding is one thing ForgeSync looked at on the node.
type Finding struct {
	// Check names what was looked at, in the words the UI shows.
	Check string `json:"check"`
	// OK is whether it is as it needs to be. Blocking says ForgeSync
	// cannot take the node on at all while it is false; a finding that is
	// not blocking is worth telling somebody about and no more.
	OK       bool   `json:"ok"`
	Blocking bool   `json:"blocking"`
	Detail   string `json:"detail"`
}

// Report is what a check of a node found.
type Report struct {
	Findings []Finding `json:"findings"`
	// Version is what the node said it runs, when it answered at all.
	Version string `json:"version,omitempty"`
	// ServiceUser is who the token turned out to belong to.
	ServiceUser string `json:"service_user,omitempty"`
	// OK is false when anything blocking failed.
	OK bool `json:"ok"`
}

// Admin adds and retires nodes. Key seals the tokens; without one a node
// cannot be added here, because its token would go into the database in
// the clear.
type Admin struct {
	DB  Store
	Key *secret.Key
	// Dial makes a client for a node. It is a field so the tests can
	// answer without a Forgejo.
	Dial func(url, token string) (NodeAPI, error)
}

// NodeAPI is what Admin asks a node about itself.
type NodeAPI interface {
	Version(ctx context.Context) (string, error)
	CurrentUser(ctx context.Context) (forgejo.User, error)
}

// ErrNoKey says the installation has no node key, so a token cannot be
// sealed and a node cannot be added from here.
var ErrNoKey = errors.New("no node key: set node_key_file on every controller, then add the node again")

func (a *Admin) dial(rawURL, token string) (NodeAPI, error) {
	if a.Dial != nil {
		return a.Dial(rawURL, token)
	}
	return forgejo.New(rawURL, token, nil)
}

// Validate checks what can be checked without leaving the controller.
func (n *NewNode) Validate() error {
	n.Name = strings.ToLower(strings.TrimSpace(n.Name))
	n.URL = strings.TrimRight(strings.TrimSpace(n.URL), "/")
	n.Site = strings.TrimSpace(n.Site)
	n.ServiceUser = strings.TrimSpace(n.ServiceUser)
	if n.ServiceUser == "" {
		n.ServiceUser = "forgesync"
	}
	var errs []error
	if !nodeName.MatchString(n.Name) {
		errs = append(errs, errors.New("the name must be lowercase letters, digits and dashes, and start and end with one of those"))
	}
	if u, err := url.Parse(n.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, errors.New("the URL must be an absolute http or https address"))
	}
	if strings.TrimSpace(n.Token) == "" {
		errs = append(errs, errors.New("a token is needed: make one for the service account on that node"))
	}
	return errors.Join(errs...)
}

// Check asks the node what it is, and reports every answer rather than
// stopping at the first: somebody setting a node up wants the whole list,
// not one thing at a time.
func (a *Admin) Check(ctx context.Context, n NewNode) (Report, error) {
	if err := n.Validate(); err != nil {
		return Report{}, err
	}
	rep := Report{OK: true}
	add := func(check string, ok, blocking bool, detail string) {
		rep.Findings = append(rep.Findings, Finding{Check: check, OK: ok, Blocking: blocking, Detail: detail})
		if blocking && !ok {
			rep.OK = false
		}
	}

	client, err := a.dial(n.URL, n.Token)
	if err != nil {
		add("The address is one ForgeSync can use", false, true, err.Error())
		return rep, nil
	}
	version, err := client.Version(ctx)
	if err != nil {
		add("The node answers", false, true, err.Error())
		return rep, nil
	}
	rep.Version = version
	add("The node answers", true, true, "Forgejo "+version)

	who, err := client.CurrentUser(ctx)
	if err != nil {
		add("The token works", false, true, err.Error())
		return rep, nil
	}
	rep.ServiceUser = who.Login
	add("The token works", true, true, "it belongs to "+who.Login)

	// Everything ForgeSync does on a node it does as a site admin: it
	// reads every repository, acts as other people through Sudo, and
	// creates accounts. A token without that is not a smaller ForgeSync,
	// it is one that fails on its first round.
	add("That account is a site admin", who.IsAdmin, true,
		map[bool]string{true: "which is what ForgeSync needs", false: "make " + who.Login + " an administrator on the node"}[who.IsAdmin])

	if who.Login != n.ServiceUser {
		add("The account is the one you named", false, false,
			fmt.Sprintf("you said %s, the token belongs to %s; ForgeSync will use %s", n.ServiceUser, who.Login, who.Login))
	} else {
		add("The account is the one you named", true, false, n.ServiceUser)
	}

	// Without this ForgeSync cannot create the SceneID account a
	// repository's owner needs, so a repository owned by somebody who has
	// not signed in there yet cannot be copied to this node. That is a
	// real limit and not a broken node, so it does not block.
	if n.SceneIDSourceID > 0 {
		add("The SceneID login source is named", true, false,
			fmt.Sprintf("id %d", n.SceneIDSourceID))
	} else {
		add("The SceneID login source is named", false, false,
			"without it ForgeSync cannot make SceneID accounts here, so a repository whose owner has never signed in on this node will wait. `forgejo admin auth list` on the node gives the id.")
	}
	return rep, nil
}

// Add checks the node and, if nothing blocking failed, seals its token
// and writes it. The node is not used until a controller picks it up.
func (a *Admin) Add(ctx context.Context, n NewNode, by string) (Report, error) {
	if a.Key == nil {
		return Report{}, ErrNoKey
	}
	rep, err := a.Check(ctx, n)
	if err != nil {
		return Report{}, err
	}
	if !rep.OK {
		return rep, nil
	}
	sealed, err := a.Key.Seal(n.Token)
	if err != nil {
		return rep, err
	}
	// The node said who the token belongs to; believe it over what was
	// typed, because that is who ForgeSync will actually be.
	user := n.ServiceUser
	if rep.ServiceUser != "" {
		user = rep.ServiceUser
	}
	err = a.DB.SaveNode(ctx, store.NodeRecord{
		Name: n.Name, URL: n.URL, Site: n.Site, ServiceUser: user,
		SceneIDSourceID: n.SceneIDSourceID, SealedToken: sealed,
		Source: "api", AddedBy: by,
	})
	return rep, err
}
