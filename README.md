# ForgeSync

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

ForgeSync keeps several independent, **stock** Forgejo servers in step, so that people can
work on whichever one is nearest and find the same thing there. It is an external control
plane: it never patches Forgejo, never touches its database, and uses only what Forgejo
offers anyone: the REST API, webhooks, the Git and LFS protocols.

It was started around [SceneGit](https://scenegit.org/), a European alternative git
repository for sceners, and the shape of the problem comes from there: several nodes in
several countries, one identity behind them, and people who should be able to push to
whichever is nearest without thinking about it.

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
[docs/LIMITATIONS.md](docs/LIMITATIONS.md), with the reason for each.

## Where to start

| If you want to | Read |
|---|---|
| Run it for real, natively on Debian | [deploy/prod/README.md](deploy/prod/README.md) |
| Try it, with five Forgejo nodes in Docker | [deploy/test/README.md](deploy/test/README.md) |
| Know what it can't do, and why | [docs/LIMITATIONS.md](docs/LIMITATIONS.md) |
| Understand the design | [docs/ForgeSync_Solution_Architecture.md](docs/ForgeSync_Solution_Architecture.md) |
| Work on the code | [CONTRIBUTING.md](CONTRIBUTING.md): the commands, the test environment, the conventions |
| Find your way around the source | [docs/CODE_REFERENCE.md](docs/CODE_REFERENCE.md): what each package does, and its entry points |
| Know how it is secured | [SECURITY.md](SECURITY.md), and [docs/SECURITY_REVIEW.md](docs/SECURITY_REVIEW.md) for the last review |

## Building

Go 1.27 and Node 22 (for the admin UI, which is embedded in the controller binary):

```sh
make build        # web UI, then bin/forgesyncd and bin/forgesync
make check        # go vet, gofmt, Go tests, UI type-check and tests
make test-db      # also the tests that need PostgreSQL
```

`bin/forgesyncd` is the controller and its admin UI; `bin/forgesync` is the command-line
client for the same API.

### Before a release

`make check` is the gate for every change. Before tagging one, these slower passes run as
well, with tools that aren't dependencies of the build -- install them where they don't
become one (`GOBIN=/tmp/bin go install ...`):

```sh
golangci-lint run ./...                      # errcheck, govet, ineffassign, staticcheck, unused
govulncheck ./...                            # known vulnerabilities in what we import
gosec -exclude-dir=web ./...                 # security patterns in the Go source
gitleaks dir . --config .gitleaks.toml       # secrets, in the tree and in the history
osv-scanner scan source -r .                 # Go and npm dependencies
shellcheck -S warning $(git ls-files '*.sh') # the setup, probe and deploy scripts
cd web && npm audit                          # and the UI's dependencies
```

`.golangci.yml` and `.gitleaks.toml` say which findings were looked at and deliberately
kept, and why: an error ignored in a test fixture, a write to an `http.ResponseWriter` with
nobody left to tell, the probe-generated usernames a secret scanner reads as keys. Keep
those files honest rather than silencing a tool in passing. `gosec` still
reports five findings by design: the session cookie's `Secure` flag is a setting (it has
to be, for a reverse proxy terminating TLS), the git CLI is run with arguments ForgeSync
builds, and the config and token files are read from paths the config gives.

## One controller, or three

A machine is always the same thing: PostgreSQL, a controller and the git cache. Controllers
share one database and take a lease in it; whichever holds the lease does the work and the
rest serve the same pages, ready to take over. Which one should be acting is configured
(`controller.priority`) and can be changed from the page of the controller you're looking
at. Sessions and ForgeSync's own accounts live in that database too, so a failover doesn't
sign anyone out.

One machine is a fine place to start. A second gives you a controller that survives losing
the first, though the database is still on one machine. **Three** is what makes the database
redundant as well, for the same reason a Proxmox cluster wants three nodes: a majority of
two is both of them, so a pair cannot promote safely. A fourth adds nothing to a quorum.
[deploy/prod/README.md](deploy/prod/README.md) section 12 has the counts, where the machines
should sit, and what a failover costs.

## Adding a node

Nodes live in ForgeSync's database, so one is added from the admin UI and every controller
has it: there is no list to edit on each machine. ForgeSync asks the node what it is and
refuses to store one it could not reach or could not use, so a mistake fails while somebody
is looking at it. The token is sealed with a key the controllers hold and the database never
sees. What ForgeSync will not do is set the node up: the parts that matter most are
`app.ini` keys no API can reach, so the UI says what the node needs and then checks.

## The scripts

| Script | What it does |
|---|---|
| `deploy/test/setup.sh` | Brings up the whole test environment: Forgejo nodes, SceneID (Keycloak), the database and one, two or three controllers. Safe to re-run. |
| `deploy/test/check-dismissal.sh` | Checks conflict dismissal end to end against a running environment: make a conflict ForgeSync can't settle, dismiss it, watch it stay dismissed, come back when it changes, and clear when it goes. |
| `deploy/test/scale.sh` | `make N` / `drop`: many repositories on one node, for measuring what a round costs. |
| `deploy/prod/backup.sh` | A dump of ForgeSync's database; `--verify` restores it into a scratch database and counts what came back. |
| `phase0/run-all.sh` | The probes that established what stock Forgejo does and doesn't allow (`phase0/README.md`). |

`deploy/prod/prometheus/` has a scrape job and alert rules for the metrics the controller
exposes.

Every script takes `PUBLIC_HOST=<host or IP>` when the environment isn't reachable under the
`*.test` names.

## This release

The first release of ForgeSync was published during **[Mysdata 2026](https://mysdata.org/)**
in Karlskrona, in co-operation with Hagar of TST. The repository is
<https://github.com/LandrethX/ForgeSync>.

## Licence

Apache License 2.0; see [LICENSE](LICENSE).

Forgejo itself is separate software under the GPL, version 3 or later, from v9 onward.
ForgeSync neither includes nor links any of it: it is a client that speaks to a Forgejo
server over the REST API, webhooks and the Git and LFS protocols, which is why the two
licences do not meet.
