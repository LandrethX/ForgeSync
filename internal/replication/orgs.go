package replication

import (
	"context"
	"sort"
	"strings"

	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/set"
	"scenegit.org/forgesync/internal/store"
)

// Organizations own repositories, so a repository owned by one can't be
// copied to a node that hasn't got it -- which is why ForgeSync used to
// leave those replicas missing. An organization's name is its identity
// everywhere, as a login is, so there is nothing to map.
//
// Three things travel. The organization itself is created where it's
// missing, owned by one of its owners (created there first if need be).
// Its profile fields merge one by one against a base, as an issue's title
// does: one new value anywhere wins everywhere, two different ones are a
// conflict for a person and nothing is written. Its teams and who is in
// them merge member by member (internal/set): a team is
// "<name>:<permission>" and a membership "<team>:<login>".
//
// Only organizations that own a repository ForgeSync knows about take
// part, and only nodes that are healthy. An organization is never deleted
// and never renamed: those are too large a thing to do on a node's word.

// OrgConflictKind is the conflict kind this owns. It's recorded against
// the organization's repositories, since that is where a person looks.
const OrgConflictKind = "org_metadata"

// orgFields are the profile fields that travel, in the order they're
// written.
var orgFields = []string{"full_name", "description", "website", "location", "visibility"}

func fieldsOf(o forgejo.Org) map[string]string {
	return map[string]string{"full_name": o.FullName, "description": o.Description,
		"website": o.Website, "location": o.Location, "visibility": o.Visibility}
}

// orgState is one node's view of an organization.
type orgState struct {
	org     forgejo.Org
	teams   map[string]forgejo.Team // by name
	members map[string]bool         // "<team>:<login>"
}

// syncOrgs keeps every organization that owns a known repository the same
// on every node. It runs once a round, after the repositories.
func (e *Engine) syncOrgs(ctx context.Context, recs []store.RepositoryRecord, healthy map[string]bool) {
	owners := map[string][]store.RepositoryRecord{}
	for _, rec := range recs {
		if rec.DeletedAt != nil {
			continue
		}
		owner, _, ok := strings.Cut(rec.FullName, "/")
		if !ok || owner == e.opts.ArchiveOrg {
			continue
		}
		owners[owner] = append(owners[owner], rec)
	}
	names := make([]string, 0, len(owners))
	for name := range owners {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if e.isOrgSomewhere(ctx, name, healthy) {
			e.syncOrg(ctx, name, owners[name], healthy)
		}
	}
}

// isOrgSomewhere reports that at least one node knows this name as an
// organization rather than a person.
func (e *Engine) isOrgSomewhere(ctx context.Context, name string, healthy map[string]bool) bool {
	for _, n := range e.order {
		if !healthy[n] || e.nodes[n].API == nil {
			continue
		}
		if is, err := e.nodes[n].API.IsOrg(ctx, name); err == nil && is {
			return true
		}
	}
	return false
}

func (e *Engine) syncOrg(ctx context.Context, name string, recs []store.RepositoryRecord, healthy map[string]bool) {
	rec, err := e.store.Org(ctx, name)
	if err != nil {
		e.log.Error("organizations: reading what was agreed failed", "organization", name, "error", err)
		return
	}
	at := map[string]*orgState{}
	unread := 0 // nodes that could not be read (see settle.go)
	for _, n := range e.order {
		if e.nodes[n].API == nil {
			continue
		}
		if !healthy[n] {
			unread++
			continue
		}
		st, found, err := e.readOrg(ctx, e.nodes[n], name)
		if err != nil {
			e.log.Warn("organizations: reading one failed", "organization", name, "node", n, "error", err)
			unread++
			continue
		}
		if found {
			at[n] = st
		} else {
			at[n] = nil // known to be missing here
		}
	}
	have := 0
	for _, st := range at {
		if st != nil {
			have++
		}
	}
	if have == 0 {
		return
	}
	e.createMissingOrgs(ctx, name, at)
	e.mergeOrgFields(ctx, name, recs, at, &rec)
	e.mergeOrgTeams(ctx, name, at, &rec, unread)
	if err := e.store.SaveOrg(ctx, rec); err != nil {
		e.log.Error("organizations: recording one failed", "organization", name, "error", err)
	}
}

func (e *Engine) readOrg(ctx context.Context, n Node, name string) (*orgState, bool, error) {
	org, found, err := n.API.GetOrg(ctx, name)
	if err != nil || !found {
		return nil, false, err
	}
	st := &orgState{org: org, teams: map[string]forgejo.Team{}, members: map[string]bool{}}
	teams, err := n.API.OrgTeams(ctx, name)
	if err != nil {
		return nil, false, err
	}
	for _, t := range teams {
		st.teams[t.Name] = t
		people, err := n.API.TeamMembers(ctx, t.ID)
		if err != nil {
			return nil, false, err
		}
		for _, u := range people {
			st.members[t.Name+":"+u.Login] = true
		}
	}
	return st, true, nil
}

// createMissingOrgs puts the organization on the nodes that haven't got
// it, owned by one of its owners.
func (e *Engine) createMissingOrgs(ctx context.Context, name string, at map[string]*orgState) {
	var from string
	var owner string
	for _, n := range e.order {
		st := at[n]
		if st == nil {
			continue
		}
		for m := range st.members {
			team, login, _ := strings.Cut(m, ":")
			if strings.EqualFold(team, "Owners") {
				from, owner = n, login
				break
			}
		}
		if owner != "" {
			break
		}
	}
	if owner == "" {
		return // nobody to own it; leave the nodes as they are
	}
	for _, n := range e.order {
		st, known := at[n]
		if !known || st != nil {
			continue
		}
		if err := e.EnsureUser(ctx, owner, from, n); err != nil {
			e.log.Info("organizations: the owner can't be created on the node; left for the next run",
				"organization", name, "node", n, "owner", owner, "error", err)
			continue
		}
		src := at[from].org
		if err := e.nodes[n].API.AdminCreateOrg(ctx, owner, forgejo.CreateOrgOption{
			UserName: name, FullName: src.FullName, Description: src.Description,
			Website: src.Website, Location: src.Location, Visibility: src.Visibility,
		}); err != nil {
			e.log.Warn("organizations: creating one failed; left for the next run", "organization", name,
				"node", n, "owner", owner, "error", err)
			continue
		}
		e.log.Info("organization created", "organization", name, "node", n, "from", from, "owner", owner)
		e.audit(ctx, "org.created_on_node", name, map[string]any{"node": n, "from": from, "owner": owner})
		if st, _, err := e.readOrg(ctx, e.nodes[n], name); err == nil {
			at[n] = st
		}
	}
}

// mergeOrgFields brings the profile fields together, one by one. Two
// different new values are a conflict, recorded against the
// organization's repositories, and nothing is written.
func (e *Engine) mergeOrgFields(ctx context.Context, name string, recs []store.RepositoryRecord,
	at map[string]*orgState, rec *store.OrgRecord) {
	base := rec.BaseFields
	if base == nil {
		base = map[string]string{}
	}
	var found []store.FoundConflict
	for _, field := range orgFields {
		vals := map[string]string{}
		for n, st := range at {
			if st != nil {
				vals[n] = fieldsOf(st.org)[field]
			}
		}
		value, writes, conflict := mergeField(vals, base[field])
		if conflict {
			for _, r := range recs {
				found = append(found, store.FoundConflict{RepositoryID: r.ID, Kind: OrgConflictKind,
					Ref:     name + " " + field,
					Details: map[string]any{"organization": name, "field": field, "values": vals}})
			}
			continue
		}
		for _, n := range writes {
			if err := e.nodes[n].API.EditOrg(ctx, name, map[string]any{field: value}); err != nil {
				e.log.Warn("organizations: writing a field failed; left for the next run", "organization", name,
					"node", n, "field", field, "error", err)
				continue
			}
			e.log.Info("organization updated", "organization", name, "node", n, "field", field, "value", value)
		}
		base[field] = value
	}
	rec.BaseFields = base
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, r.ID)
	}
	if _, err := e.store.SyncConflicts(ctx, found, ids, []string{OrgConflictKind}, e.now()); err != nil {
		e.log.Error("organizations: recording conflicts failed", "organization", name, "error", err)
	}
}

// mergeOrgTeams brings the teams and who is in them together, member by
// member. Owners is never removed: every organization has one, and Forgejo
// won't let it go.
func (e *Engine) mergeOrgTeams(ctx context.Context, name string, at map[string]*orgState, rec *store.OrgRecord, unread int) {
	teams := map[string]map[string]bool{}
	members := map[string]map[string]bool{}
	for n, st := range at {
		if st == nil {
			continue
		}
		teams[n], members[n] = map[string]bool{}, st.members
		for tn, t := range st.teams {
			teams[n][tn+":"+t.Permission] = true
		}
	}
	if len(teams) < 2 {
		return
	}
	// Teams first: a membership needs its team to be there.
	plan := set.Decide(teams, set.From(rec.BaseTeams))
	for _, n := range e.order {
		st := at[n]
		if st == nil {
			continue
		}
		for _, m := range plan.Add[n] {
			tn, perm, _ := strings.Cut(m, ":")
			if strings.EqualFold(tn, "Owners") {
				continue // every organization has one already
			}
			src, ok := teamNamed(at, tn)
			if !ok {
				continue
			}
			src.Permission = perm
			made, err := e.nodes[n].API.CreateTeam(ctx, name, src)
			if err != nil {
				e.log.Warn("organizations: creating a team failed; left for the next run", "organization", name,
					"node", n, "team", tn, "error", err)
				continue
			}
			st.teams[tn] = made
			teams[n][m] = true
			e.log.Info("team created", "organization", name, "node", n, "team", tn, "permission", perm)
			e.audit(ctx, "org.team_created", name, map[string]any{"node": n, "team": tn, "permission": perm})
		}
		for _, m := range plan.Remove[n] {
			tn, _, _ := strings.Cut(m, ":")
			if strings.EqualFold(tn, "Owners") {
				continue
			}
			t, ok := st.teams[tn]
			if !ok {
				continue
			}
			if err := e.nodes[n].API.DeleteTeam(ctx, t.ID); err != nil {
				e.log.Warn("organizations: removing a team failed; left for the next run", "organization", name,
					"node", n, "team", tn, "error", err)
				continue
			}
			delete(st.teams, tn)
			delete(teams[n], m)
			e.log.Info("team removed", "organization", name, "node", n, "team", tn)
			e.audit(ctx, "org.team_removed", name, map[string]any{"node": n, "team": tn})
		}
	}
	rec.BaseTeams = settle(unread, rec.BaseTeams, teams, set.From(rec.BaseTeams))

	// Then who is in them.
	from := ""
	for _, n := range e.order {
		if at[n] != nil {
			from = n
			break
		}
	}
	plan = set.Decide(members, set.From(rec.BaseMembers))
	for _, n := range e.order {
		st := at[n]
		if st == nil {
			continue
		}
		for _, m := range plan.Remove[n] {
			tn, login, _ := strings.Cut(m, ":")
			t, ok := st.teams[tn]
			if !ok {
				continue
			}
			if err := e.nodes[n].API.RemoveTeamMember(ctx, t.ID, login); err != nil {
				e.log.Warn("organizations: taking someone out of a team failed; left for the next run",
					"organization", name, "node", n, "team", tn, "who", login, "error", err)
				continue
			}
			delete(members[n], m)
			e.log.Info("team member removed", "organization", name, "node", n, "team", tn, "who", login)
			e.audit(ctx, "org.member_removed", name, map[string]any{"node": n, "team": tn, "who": login})
		}
		for _, m := range plan.Add[n] {
			tn, login, _ := strings.Cut(m, ":")
			t, ok := st.teams[tn]
			if !ok {
				continue // its team isn't there yet; next run
			}
			if err := e.EnsureUser(ctx, login, from, n); err != nil {
				e.log.Info("organizations: the person can't be created on the node; left for the next run",
					"organization", name, "node", n, "who", login, "error", err)
				continue
			}
			if err := e.nodes[n].API.AddTeamMember(ctx, t.ID, login); err != nil {
				e.log.Warn("organizations: putting someone in a team failed; left for the next run",
					"organization", name, "node", n, "team", tn, "who", login, "error", err)
				continue
			}
			members[n][m] = true
			e.log.Info("team member added", "organization", name, "node", n, "team", tn, "who", login)
			e.audit(ctx, "org.member_added", name, map[string]any{"node": n, "team": tn, "who": login})
		}
	}
	rec.BaseMembers = settle(unread, rec.BaseMembers, members, set.From(rec.BaseMembers))
}

// mergeField is the rule used for every single value ForgeSync merges: one
// new value anywhere wins everywhere, two different ones are a conflict and
// nothing is written.
func mergeField(vals map[string]string, base string) (value string, writes []string, conflict bool) {
	changed := map[string]bool{}
	for _, v := range vals {
		if v != base {
			changed[v] = true
		}
	}
	switch len(changed) {
	case 0:
		return base, nil, false
	case 1:
		for v := range changed {
			value = v
		}
		for node, v := range vals {
			if v != value {
				writes = append(writes, node)
			}
		}
		sort.Strings(writes)
		return value, writes, false
	}
	return "", nil, true
}

// teamNamed is a team as some node has it, to copy elsewhere.
func teamNamed(at map[string]*orgState, name string) (forgejo.Team, bool) {
	for _, st := range at {
		if st == nil {
			continue
		}
		if t, ok := st.teams[name]; ok {
			return t, true
		}
	}
	return forgejo.Team{}, false
}
