# ForgeSync

ForgeSync keeps several independent, **stock** Forgejo servers in step, so that people can
work on whichever one is nearest and find the same thing there. It is an external control
plane: it never patches Forgejo, never touches its database, and uses only what Forgejo
offers anyone — the REST API, webhooks, the Git and LFS protocols.

It is built around one rule: **never lose work, never decide for the owner.** Only
fast-forwards and creations are pushed, every push is leased against what ForgeSync last
wrote, and anything two people could genuinely disagree about becomes a conflict with both
sides shown rather than a decision made quietly.

## What it keeps the same

Branches and tags, the LFS objects large files are kept in, the wiki, releases and their
files, repository settings and topics, branch protection (plus ForgeSync's own guard on the
replicas), collaborators, organizations with their teams and members, labels, milestones,
issues and comments, pull requests and their conversation, reviews, reactions, attachments,
assignees, Actions variables, generic and maven packages, and forks as forks. A repository
missing on a node is created, with its SceneID owner; one deleted on its primary is archived
on the others rather than deleted; a rename keeps the repository's identity.

What it deliberately doesn't, and what Forgejo won't let it, is in
[docs/LIMITATIONS.md](docs/LIMITATIONS.md) — with the reason for each.

## Where to start

| If you want to | Read |
|---|---|
| Run it for real, natively on Debian | [deploy/prod/README.md](deploy/prod/README.md) |
| Try it, with five Forgejo nodes in Docker | [deploy/test/README.md](deploy/test/README.md) |
| Know what it can't do, and why | [docs/LIMITATIONS.md](docs/LIMITATIONS.md) |
| Understand the design | [docs/ForgeSync_Solution_Architecture.md](docs/ForgeSync_Solution_Architecture.md) |
| Work on the code | [CLAUDE.md](CLAUDE.md) — the layout, the decisions and why |

## Building

Go 1.27 and Node 22 (for the admin UI, which is embedded in the controller binary):

```sh
make build        # web UI, then bin/forgesyncd and bin/forgesync
make check        # go vet, gofmt, Go tests, UI type-check and tests
make test-db      # also the tests that need PostgreSQL
```

`bin/forgesyncd` is the controller and its admin UI; `bin/forgesync` is the command-line
client for the same API.

## Two controllers, one database

A pair of controllers share one PostgreSQL database and take a lease in it; whichever holds
the lease does the work and the other serves the same pages, ready to take over. Which one
should be acting is configured (`controller.priority`) and can be changed from the page of
the controller you're looking at. Sessions and ForgeSync's own accounts live in that
database too, so a failover doesn't sign anyone out.

## The scripts

| Script | What it does |
|---|---|
| `deploy/test/setup.sh` | Brings up the whole test environment: Forgejo nodes, SceneID (Keycloak), the database and one or two controllers. Safe to re-run. |
| `deploy/test/check-dismissal.sh` | Checks conflict dismissal end to end against a running environment: make a conflict ForgeSync can't settle, dismiss it, watch it stay dismissed, come back when it changes, and clear when it goes. |
| `deploy/test/scale.sh` | `make N` / `drop`: many repositories on one node, for measuring what a round costs. |
| `deploy/prod/backup.sh` | A dump of ForgeSync's database; `--verify` restores it into a scratch database and counts what came back. |
| `phase0/run-all.sh` | The probes that established what stock Forgejo does and doesn't allow (`phase0/README.md`). |

`deploy/prod/prometheus/` has a scrape job and alert rules for the metrics the controller
exposes.

Every script takes `PUBLIC_HOST=<host or IP>` when the environment isn't reachable under the
`*.test` names.
