# What ForgeSync doesn't do, and why

Three kinds of thing are in this list: what Forgejo won't let anyone do, what ForgeSync
could do but shouldn't, and what simply hasn't been built yet. They're marked, because
"can't" and "not yet" deserve different answers from you.

Everything here was established against Forgejo v16 by running it, usually in
`phase0/` or against the five-node test environment, not by reading documentation.

---

## Can't be done: Forgejo doesn't offer it

### Projects (the boards)
**There is no API.** Forgejo v16 has no REST endpoints for projects, columns or cards:
`/repos/{owner}/{repo}/projects` answers 404 and the v16 router has no project routes at
all. Nothing can copy them without reaching into Forgejo's database, which ForgeSync is not
allowed to do.

*What you can do:* nothing, until Forgejo adds an API. A board is per-node.

### Actions secrets
**Write-only by design.** Forgejo gives back a secret's name and when it was set, never its
value, which is the correct behaviour for a secret store. No client can copy one, and
neither can ForgeSync.

*What ForgeSync does instead:* it notices a node that hasn't got a secret the others have
and reports it (`actions_secret_missing`), so the gap is visible before a workflow fails
there rather than after. The conflict names the nodes to set it on. If you'd rather not be
told, it can be dismissed.

### Package types other than generic and maven
**Each registry is its own protocol.** A package travels only if its files can be fetched
and put back by path: generic and maven can. Container images speak the OCI distribution
protocol (manifests, blob upload sessions, tags); npm, nuget, pypi, rubygems and the rest
each take an upload in their own shape and build an index out of it. Copying those
faithfully means implementing each client, and a half-copied package is worse than none.

*What ForgeSync does instead:* a package of a type it can't publish, that isn't on every
node, is reported once (`package_unreplicated`) naming the type and the nodes without it.

### Personal access tokens, deploy keys
**Tokens are secrets** (see above) and deploy keys belong to whoever created them for one
node. Copying either would spread credentials ForgeSync was never given.

*What you can do:* create them per node, as their owners.

---

## Won't be done: it would be wrong

### Pull mirrors
A pull mirror already keeps itself up to date from somewhere else, and Forgejo makes it
read-only, so there is nothing for ForgeSync to push into. Making a second mirror on another
node would need whatever credentials the first one uses, which ForgeSync doesn't hold and
shouldn't ask for, and would double the load on whoever is being mirrored.

*What ForgeSync does instead:* the replica stays `missing` with the upstream named in the
reason, so a person can set the same mirror up if that's what they want.

### `has_issues`, `has_releases`, `has_actions` and the rest of a node's own switches
Whether a node runs workflows, takes issues or shows releases is a decision about **that
node**. Five nodes running the same pipeline on every push is rarely what anyone wants, and
turning a tab on everywhere would promise what isn't there. ForgeSync replicates the
contents, not the switches; where a switch is off, its endpoints answer 404 and ForgeSync
passes that node over quietly.

*Exception:* `has_wiki` joins the merge when `replication.wiki` is on, because then the
wiki's pages really do travel and the tab means something.

### The default branch, and `archived`
The default branch already follows the primary through replication, and a difference is the
detector's own `default_branch_mismatch`; merging it as metadata as well would fight itself.
`archived` would stop replication to any node that took it, and ForgeSync's own archive flow
owns that flag.

### Making a repository public
Private travels one way: a repository private on **any** node is made private on all of
them, and ForgeSync never makes one public. What has been public may already have been read;
that isn't a decision to make automatically.

### Resolving a diverged branch
Two people committed different work. ForgeSync will hand it to the repository's owner as a
pull request on the primary and act on their answer, but it will not choose, and it will
never force-push over history on its own judgement.

---

## Not yet: could be done, hasn't been

### PostgreSQL is a single point of failure
The controllers fail over; the database doesn't. ForgeSync behaves well without it: a
controller that can't renew its lease stops acting *before* the lease expires, the pages
stay up, and `/metrics` reports `forgesync_database_up 0`. But nothing is synced while it
is down.

*What to do about it:* put something under it, a managed PostgreSQL with failover or
streaming replication with a promotion tool, and point both controllers at whatever fronts
it. They reconnect by themselves; there is nothing to restart afterwards.
[deploy/prod/README.md](../deploy/prod/README.md) has the options.

### Scale beyond 200 repositories
Measured at 203 repositories on each of five nodes: a push reaches all four replicas in
about five seconds (that path replicates one repository, told by a webhook), and a full
round takes about three minutes. The round grows with repositories times nodes, so at a few
thousand it would need an interval longer than the default and the per-item passes (issues,
reactions, attachments, at one API call per item per node) would want looking at. Nobody has
run it at that size.

*What to do about it:* keep `inventory.interval` comfortably above what a round takes (a
round logs its own duration; `log.level: debug` logs each part), and lean on the webhooks,
which is where the speed actually is.

### Not run in production
Everything here has been exercised against five Forgejo nodes in a test environment,
including failover, restore, and starting from an empty database. It has not yet run
anywhere real, which is the one thing a 1.0 should be able to claim.

---

## What happens when something can't be replicated

ForgeSync never pretends. Anything it can't carry, or can't decide, comes back as a conflict
naming the repository, the nodes and the reason, and stays until it's settled or dismissed.
A dismissed conflict is kept with who dismissed it and why, stops being counted, and comes
back if what it says changes, so the number on the dashboard is one somebody can actually
bring to zero.
