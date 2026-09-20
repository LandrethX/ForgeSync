# ForgeSync code reference

What each part of the code does, in the order the work happens. It describes the
program as built, while `docs/ForgeSync_Solution_Architecture.md` describes the design
it was built from and `docs/LIMITATIONS.md` says where the two still differ.

Every exported type and function carries a doc comment, so the detail of any one
signature is a command away:

```sh
go doc ./internal/replication          # the package summary and its API
go doc ./internal/replication.Plan     # one function, with its comment
go doc -all ./internal/store           # everything the package exports
```

This file is the map that tells you which package to point that at.

## One round, end to end

A controller does the same loop whether it was woken by a timer or by a node:

1. **Scan.** `inventory.Scanner` lists every repository on every node and reads each
   default branch head. A round is complete when every node's latest scan succeeded.
2. **Rename detection.** `store.DetectRenames` gives a repository its new name when
   the same Forgejo id turns up under another, keeping the old name as an alias.
3. **Primaries.** `inventory.AssignPrimaries` gives every user and repository a primary
   site, by the rules in the architecture document: a user's registration site, a
   repository's owner's site, or the node that had it first.
4. **Conflicts.** `conflicts.Detector` asks each node's Forgejo whether the others'
   heads are contained in its own, and records what no automatic rule may settle.
5. **Replication.** `replication.Engine` copies each repository from its primary to the
   other nodes, then the things that hang off a repository (issues, releases, packages
   and the rest), each behind its own switch.
6. **Issues.** `issues.Syncer` merges conversations in both directions.

A webhook delivery short-circuits the loop: `webhook.Receiver` hands one repository to
`webhook.RepoDispatcher`, which replicates that repository alone and rescans only it.
Full rounds stay as the safety net.

Only the controller holding the leadership lease does any of this. Everything above
runs under `leader.Elector.Supervise`, whose context is cancelled the moment leadership
goes.

## Binaries

### `cmd/forgesyncd`

The controller. `run` loads the config, opens and migrates the database, builds one
`forgejo.Client` per node, and wires the packages below together: the health monitor,
the scanner, the conflict detector, the replication engine, the issue syncer, the
webhook installer and receiver, and the HTTP server. It then serves plain HTTP, HTTPS,
or both, and re-reads the TLS certificate on SIGHUP.

Three adapters in this package keep the standby quiet and the packages independent of
each other: `leaderRecorder` (both controllers watch the nodes, only the leader records
the transitions), `leaderDispatcher` (a delivery to a standby is dropped, since the
leader had the same one), and `userAdmin` (lets the API create users without importing
the replication package's options). `recordController` writes this controller's
heartbeat on the same beat as the lease, leader or not, so the dashboard can show both.

### `cmd/forgesync`

The CLI for the admin API, built with Cobra: `node list`, `repo list`,
`repo set-primary`, `user list`, `user set-home`, `conflict list` and `history`, the
last with filters, paging, `--export csv|json` and `-f` to follow new entries. It
authenticates with the admin token as a bearer token, which counts as Administrator.
The command implementations are in `internal/cli`.

## Packages

### `internal/config`

Loads and validates the YAML configuration. Unknown keys are an error, so a mistyped
setting is reported rather than silently doing nothing. Tokens are always read from
files named in the config and resolved relative to it, so the config itself can be
shared. Defaults are applied in one place (`applyDefaults`) and the result is validated
before anything else starts.

| Entry point | What it does |
|---|---|
| `Load(path)` | Reads the file, applies defaults, validates, and reads every token file. |
| `HTTP.TLS/PlainListen/HTTPSListen` | Which addresses are served, and over what. |
| `HTTP.ProxyPrefixes()` | Parses `http.trusted_proxies` into prefixes for the API. |
| `Config.Nodes` | The Forgejo servers, each with its URL, token file, service account and SceneID source id. |

### `internal/forgejo`

A small Forgejo REST client: token authentication, `Sudo` to act as another user, and a
typed `APIError` with `IsNotFound`, `IsConflict` and `IsAuthError` for the cases the
callers actually branch on. It covers only what ForgeSync uses, which is most of the
v16 API surface for repositories, issues, pull requests, releases, packages, teams and
administration.

A feature switched off on a node answers 404 from its endpoints, so `IsNotFound` is read
as "there are none here" by every pass that lists something optional.

### `internal/auth`

Identities and roles. `Role` is an ordered `Viewer < Operator < Administrator`, which is
what `requireRole` compares against. `Identity` is who a request is from and where that
was decided (an account, the admin token), and `Actor()` is how it appears in the audit
log. `RoleMapping` resolves a role from a configurable OIDC claim, including dotted
paths, for installations that map SceneID groups onto ForgeSync roles.

### `internal/store`

Every piece of ForgeSync's own state, in PostgreSQL through pgx. Migrations are embedded
`migrations/NNNN_name.sql` files applied in order under an advisory lock; an applied
migration is never edited, a new one is added.

| Area | Entry points | Tables |
|---|---|---|
| Nodes and scans | `SyncNodes`, `NodeStates`, `RecordNodeStatus`, `NodeScans`, `RecordNodeScanFailure` | `nodes`, `node_state_transitions`, `inventory_scans` |
| Repositories | `RecordNodeScan`, `RecordRepoObservation`, `Repositories`, `Repository`, `DetectRenames`, `SetPrimary` | `repositories`, `repository_replicas`, `repository_aliases`, `repository_archives` |
| Users | `Users`, `SetUserHome`, `AssignPrimaries` support | `users`, `user_accounts`, `created_accounts` |
| Conflicts | `SyncConflicts`, `Conflicts`, `AcknowledgeConflict`, `DismissConflict`, `ReopenConflict` | `conflicts` |
| Replication state | `ReplicaSyncs`, `ReplicatedRefs`, `WikiRefs`, the handoff records | `replica_sync`, `replicated_refs`, `wiki_refs`, `conflict_handoffs`, `organizations`, `package_owners` |
| Conversations | `Issues`, issue and comment copies | `issues`, `issue_copies`, `issue_comments`, `issue_comment_copies`, `repo_items`, `repo_item_copies` |
| Leadership | `AcquireLease`, `Leadership`, `ReleaseLease`, `Choose`, `ClearChoice` | `leadership`, `leadership_choice`, `controllers` |
| Accounts and sessions | `CreateAccount`, `CheckPassword`, `HashPassword`, `VerifyPassword`, `CreateSession`, `Session`, `DeleteSession` | `accounts`, `sessions` |
| History | `Audit`, `History`, `HistoryEach`, `HistoryActors` | `audit_log` (unioned with `node_state_transitions`) |

`Open` and `Ping` mean "this database takes writes", not merely "something answered".
Where the database is more than one server, a controller can end up talking to a standby:
it would answer every read, so the pages would look right, while the lease could not be
renewed and nothing was replicated. `Ping` asks the server which it is
(`pg_is_in_recovery`) and returns `ErrStandby`, so `Open` refuses to start and `/readyz`,
`/metrics` and the overview say so if it happens later.

`HashPassword` is PBKDF2-HMAC-SHA256 with a 16-byte salt and the iteration count stored
in the string, so the cost can be raised without invalidating what exists.
`VerifyPassword` compares in constant time, and `CheckPassword` hashes even when the
username does not exist, so a refusal costs the same either way.

`History` is one timeline built by a `UNION ALL` of the audit log and the node
transitions. Filters run in SQL, paging uses a keyset cursor, and every value reaches
the query as a placeholder.

### `internal/leader`

Leadership as a lease row in the shared database, so the database is the witness and no
second channel can disagree with it. A controller works only while it holds a lease that
has not run out by its own clock, measured from when it sent the renewal: a leader that
loses the database stops on its own before the lease expires, and another can take over
only after it has. The fencing is the lease running out, not either side noticing.

| Entry point | What it does |
|---|---|
| `New(store, opts, log)` | Builds the elector: holder id, name, URL, lease, renew interval, yield rule. |
| `Elector.Run` | Takes and renews the lease, and gives it up on a planned stop. |
| `Elector.Supervise` | Runs leader-only work under a context cancelled the moment leadership goes. |
| `Elector.Leading`, `State` | Whether this controller acts, and what the UI shows. |

`Options.Yield` is how a controller hands the lease back to a better one
(`controller.priority`, lowest first). A heartbeat alone does not earn a hand-over: the
one written a moment before a controller stopped still looks recent, so the yield rule
waits to see a heartbeat move between two renewals.

### `internal/health`

`Next` is the pure state machine, table-tested: one failed contact makes a node SUSPECT,
`failure_threshold` consecutive failures make it UNREACHABLE, a success makes it
HEALTHY. `Monitor` runs the contacts and reports changes to a `Recorder`.
`Monitor.Restore` seeds it with the nodes' last known states at startup, so restarting a
controller is not recorded as every node changing from UNKNOWN to HEALTHY.

### `internal/inventory`

Discovery, and the primary-site rules.

| Entry point | What it does |
|---|---|
| `NewScanner`, `Scanner.Run` | Lists every repository on every node, 50 per page, and reads each default branch head with `branch_concurrency` in parallel. |
| `Scanner.ScanRepo` | One repository on every node, for the webhook fast path, including its previous name. |
| `Options.AfterScan` | The hook the controller hangs the rest of a round on: primaries, then conflicts, then replication. |
| `AssignPrimaries` | Registration site for a user, the owner's site for their repositories, earliest `created_at` for anything else. An Administrator's choice is never replaced. |
| `Compare` | same / differs / missing / deleted / unknown per repository, for the UI and the CLI. |

The scanner only observes. What it reports is what the nodes say, not what ForgeSync
has done about it, and the UI says so.

### `internal/conflicts`

Runs after every scan round. For each repository's default branch it asks each node's
Forgejo, through the compare API, whether the other node's head is contained in its own.
Each side having commits the other lacks opens a `git_diverged` conflict; one node merely
behind is not a conflict. Different default branches open `default_branch_mismatch`.

A conflict clears automatically once a later check that is conclusive (fresh data, every
comparison answered) no longer finds it. The detector never changes data to resolve one.
Operators can acknowledge a conflict with a note, or dismiss it: a dismissed conflict
keeps its row and its note, stops being counted, and comes back if what it says changes.

Ownership matters: `SyncConflicts` clears only the kinds the caller owns. With
replication on, the engine owns the `git_*` kinds for repositories that have a primary,
and the detector keeps `default_branch_mismatch` and the repositories with no primary.

### `internal/replication`

The largest package, one file per kind of content. Everything here is idempotent: a run
that was interrupted costs nothing but the work it repeats.

| File | What it replicates |
|---|---|
| `plan.go` | `Plan`, pure and table-tested: per ref, from primary, replica and base, it allows only create, fast-forward, and delete-what-we-wrote. Anything else is an `Issue` that becomes a conflict. |
| `git.go` | `Git` runs the git CLI in a bare cache per repository. Every push is `--force-with-lease`, the token goes in an `http.extraHeader` from the environment, never in arguments or on disk, and results are parsed per ref from `--porcelain`. |
| `engine.go` | `Engine.RunAll` over every repository, `Engine.RunRepo` for one, `Engine.Trigger` for the webhook path (which runs again after the current run if it is busy). Inside `runOnce`: decisions first, then replication, then fixes, and a fix that moved the primary triggers a second pass. |
| `provision.go` | Creating a missing repository, its SceneID owner, or a fork as a fork. A pull mirror is never created; the replica stays missing with the upstream named. |
| `handoff.go` | `autoFix`, `handOff`, `applyDecisions`: the fixes that lose nothing, and the pull request that asks a repository's owner about a diverged branch. |
| `deletion.go` | A repository deleted on its primary is archived on the others, never deleted, and purged after `backup_days`. |
| `rename.go` | Renames and transfers each copy before replicating to it. |
| `wiki.go`, `releases.go`, `packages.go`, `lfs.go`, `actions.go`, `metadata.go`, `protection.go`, `orgs.go`, `collaborators.go`, `users.go` | One kind of content each, described in `README.md` and in each file's own comment. |

`Plan` is where the safety property lives: an unknown ancestry counts as diverged, and
a replica that has changed under ForgeSync is never overwritten. The tests run against a
real `git http-backend` rather than mocks, because the property being tested is git's
behaviour.

### `internal/set`

The merge for things that are sets rather than single values: collaborators, reactions,
attachments, reviews, teams, labels on an issue. A member is either there or not, and
the base says which way it moved, so one that appears anywhere is added everywhere and
one that is missing anywhere is taken away everywhere. Two people cannot disagree about
the same member, so this kind of merge never becomes a conflict.

`Members` and `Settle` are the whole of it: what to write now, and what the base becomes.
A member enters the base only once every node has it and leaves only once none has,
which is what makes a write ForgeSync could not do get tried again rather than read as
someone's deletion.

### `internal/issues`

Issue, comment, pull request, review, reaction and attachment replication. Each issue
and comment has one identity and a copy per node, because Forgejo assigns numbers
itself. Copies are made in creation order so numbers line up when they can, and
differing numbers are recorded and shown rather than hidden.

Title, body, state and comment bodies merge per field against a base: one new value
anywhere wins everywhere, two different ones are an `issue_conflict` and nothing is
written. Everything is written as the person who wrote it, through `Sudo`; edits are the
service account's. A pull request's state never merges: a copy is closed once the
primary's is closed or merged, and ForgeSync never merges or reopens one, because
merging the same pull request twice makes two different merge commits.

Loops end by comparison: the webhook for ForgeSync's own write leads to a run that finds
nothing to do.

### `internal/webhook`

The fast path. `Installer` keeps exactly one ForgeSync system webhook per node for
push, create, delete and repository events. Each node signs with `NodeSecret(secret,
node)`, and the hook URL carries a fingerprint of that secret, so changing the secret
replaces the hook rather than leaving a stale one.

`Receiver` checks the HMAC before parsing anything, ignores the service account's own
changes, and hands the rest to `RepoDispatcher`: a known repository is replicated at
once, an unknown one is scanned first. `Tracker` counts deliveries and rejections per
node for the webhook page and the metrics.

### `internal/api`

The HTTP interface: a chi router with `/healthz`, `/readyz`, `/metrics` and `/api/v1/*`,
plus the embedded UI underneath.

| Concern | Where |
|---|---|
| Routing and roles | `server.go`: `Server.Handler`, `requireLeader`, `securityHeaders`, `noStore` |
| Authentication | `auth.go`: bearer token or session cookie, the CSRF header, the sign-in endpoints, `clientAddr` |
| Sessions | `session.go`: `Sessions` over the shared database, and the sign-in rate limiter |
| ForgeSync's own accounts | `accounts.go` |
| Resources | `repos.go`, `users.go`, `conflicts.go`, `replication.go`, `history.go`, `metrics.go` |

Every database read in `metrics.go` shares one two-second budget, well inside the
handler's own. A scrape is at its most useful when the database is unreachable, which is
when `forgesync_leader` and the node series say who is acting and what they can still
see, and each read would otherwise sit through a connection attempt to every server named
in the URL. A scrape answers with what it has; it never hangs.

Reads need Viewer. `/history`, its export, `POST /inventory/scan`, the conflict verbs and
`POST /repositories/{id}/replicate` need Operator. Setting a primary or a home node,
managing accounts and choosing the leader need Administrator. Writes that change a node
are behind `requireLeader`, and a standby answers 409 naming the leader; the writes that
are only ForgeSync's own bookkeeping (accounts, the leadership choice, a conflict note or
dismissal) are deliberately not, because being locked out of one controller is exactly
when the other has to work. `internal/api/security_test.go` holds that matrix as a test.

A session cookie is HttpOnly, SameSite=Strict, Secure unless configured otherwise, and
what is stored is the SHA-256 of its value. A cookie-authenticated write needs the
`X-ForgeSync-CSRF` header, which another site cannot set without a preflight this server
never allows.

### `internal/webui` and `web/`

`webui.Handler` serves the UI built by Vite into `internal/webui/dist` and embedded in
the binary, with an SPA fallback and a plain 503 with a hint when there is no build. The
UI itself is React and TypeScript with no router or UI library; `web/src/router.tsx` is
the whole of the routing. A standby serves every page, so `useLeadershipPoll` asks the
overview which controller this is and every button that writes is disabled with a title
saying why. The content security policy forbids inline scripts and styles.

### `internal/cli` and `internal/buildinfo`

`cli` holds the Cobra commands and the small HTTP client they share, including the
history follower and the CSV and JSON exporters. `buildinfo` carries the version and
commit stamped in at build time.

## Where the tests are

Each package's tests sit beside it. The ones worth knowing about:

- `internal/replication/gitserver_test.go` runs a real `git http-backend`, an LFS batch
  endpoint and a package registry, so the safety properties are tested against git and
  the real protocols rather than mocks.
- `internal/replication/plan_test.go` and `internal/health/health_test.go` are table
  tests over pure functions, which is where most of the decision logic lives.
- `internal/api/security_test.go` is the access-control matrix: every write endpoint
  against nobody signed in, each role below the one it needs, and the role itself.
- `internal/store` and `internal/leader` have tests that need a real PostgreSQL; they
  share one database, so run them with `-p 1`. See `CONTRIBUTING.md`.
