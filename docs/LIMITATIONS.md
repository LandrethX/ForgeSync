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

### Package types other than generic, maven, nuget, rubygems and helm
**Each registry is its own protocol.** A package travels only if ForgeSync can fetch its
files and put them back, and end up with the whole package rather than part of one.
Five types manage it. generic and maven read and write at the same path. nuget, rubygems
and helm are read by path and uploaded to one endpoint that works out for itself what it
was given, which is safe because each is a single-file package: putting that file back is
the whole of it.

The rest are not. Container images speak the OCI distribution protocol (manifests, blob
upload sessions, tags). npm and composer wrap the file in JSON, pypi in a form carrying
its own metadata, and debian and rpm need a distribution and component the package API
never reports. Copying any of them faithfully means implementing that client, and a
half-copied package is worse than none.

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

### One PostgreSQL server is a single point of failure
The controllers fail over; one database server does not. ForgeSync behaves well without it: a
controller that can't renew its lease stops acting *before* the lease expires, the pages stay
up, and `/metrics` reports `forgesync_database_up 0`. But nothing is synced while it is down.

This is no longer only advice. ForgeSync follows a promotion by itself: name every server in
`database.url` with `target_session_attrs=read-write` and each connection goes to whichever
one takes writes, so a promotion needs no proxy, no restart and no address changed. It also
refuses to work in a standby, which answers every read and accepts no write, rather than
reporting itself healthy while nothing is being replicated.

That was measured, not reasoned about: `deploy/test/check-db-failover.sh` kills the primary of
a two-server database under Patroni and watches the controllers through it. The leader stops
acting 8 to 9 seconds in, inside its 10 second lease and with nothing having taken over; a
new primary is promoted about 20 seconds in; a controller is leading again 2 to 3 seconds
after that; and the repositories, replicas and conflicts are the same on the other side. A
planned switchover is over inside a second, without leadership dropping at all.

*What's left:* ForgeSync doesn't install or manage any of it. Making the database redundant
takes **three** machines, not two, for the same reason a Proxmox cluster wants three nodes: a
majority of two is both of them, so a pair cannot promote safely. `deploy/prod/README.md`
has the counts, the placement, and what a failover costs.

### Automatic promotion of the database needs three machines, not two
A second machine can keep a continuously updated copy of the database (`deploy/prod/standby.sh`),
so losing the first loses nothing. Promoting it is deliberately a person's decision, because
with two machines nothing can tell "the other one is dead" from "I cannot reach the other
one", and a pair that promotes on its own judgement ends up, in a partition, with two
primaries and two histories that will not merge.

*What to do about it:* three machines, where a majority of two makes the decision safely.
`deploy/prod/cluster.sh` installs that arrangement (etcd and Patroni, from Debian's own
packages) and `deploy/prod/README.md` section 12 says where the machines should sit. Measured
on three: the primary machine stopped outright, another promoted in about half a minute with
nobody doing anything, the surviving controllers never went down, and the dead machine
rejoined as a replica five seconds after it came back.

What is still true is that ForgeSync does not watch the database for you. Patroni does, and
`forgesync_database_up` tells you when a controller cannot reach it, but nothing here alerts
on replication lag or on a machine that has been out of the cluster for a week. That belongs
to whatever watches your PostgreSQL.

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

## What happens when a node cannot be reached

Nothing is decided without it. The things that merge as sets -- packages, collaborators,
branch protection, releases, topics, Actions variables, an organization's teams and members,
an issue's reactions and attachments -- remember what the nodes last agreed on, and that
record only moves once every node that has the thing has been read. A node that is down
therefore holds it still rather than having its silence read as an answer, which is what
stops a package published while it was away from being deleted everywhere when it comes
back, and stops access granted meanwhile from being revoked.

The cost is that a real deletion waits too: something deleted while a node is unreachable
reaches the other nodes once that node can be read again. That is the trade this project
makes everywhere, and it is the same one as never force-pushing over divergent history.
