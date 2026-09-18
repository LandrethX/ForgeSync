# ForgeSync --- Distributed Forgejo Synchronisation and Control Plane

**Status:** Solution architecture / initial requirements\
**Purpose:** Define a service that keeps multiple independent Forgejo
instances synchronised while allowing users to use any participating
Forgejo node.\
**Initial topology:** Two Forgejo nodes and one ForgeSync controller,
with an optional secondary ForgeSync controller.\
**Design principle:** ForgeSync must work with standard Forgejo
installations and must not require patched Forgejo binaries, custom
database tables, direct database writes, or proprietary Forgejo
modifications.

------------------------------------------------------------------------

## 1. Executive summary

ForgeSync is proposed as an external control plane for multiple
independent Forgejo installations.

Each Forgejo server remains a normal, independently usable Forgejo
instance. ForgeSync monitors the nodes, tracks repository and
application state, detects changes, synchronises supported objects,
detects conflicts, manages repository-primary designation, and provides
a central status and administration interface.

The intended model is:

``` text
                         SceneID
                    Common user identity
                           |
                           v
                    ForgeSync service
                  sync.scenegit.org
                           |
                 +---------+---------+
                 |                   |
                 v                   v
          ForgeSync Primary    ForgeSync Secondary
              LEADER             FOLLOWER
                 |                   |
                 +---------+---------+
                           |
                    ForgeSync state
                      PostgreSQL
                           |
             +-------------+-------------+
             |             |             |
             v             v             v
         Forgejo SE    Forgejo DK    Forgejo DE
```

ForgeSync is conceptually similar to a DNS/control-plane service in that
it maintains knowledge about all Forgejo nodes, their health, roles and
repository ownership. It does **not**, however, replace DNS itself. DNS
or a reverse proxy can optionally consume ForgeSync health/role
information to direct users to an appropriate Forgejo node.

A repository has a designated **primary Forgejo node**. Other nodes
contain synchronised replicas. Users may access any node, but the
primary provides an authoritative point for conflict resolution and
ordered state changes.

ForgeSync itself may run as:

-   one controller for a simple installation; or
-   a primary/leader controller plus an optional secondary/follower for
    high availability.

The secondary ForgeSync controller is strongly recommended for
production environments but is not required for the first deployment.

------------------------------------------------------------------------

## 2. Objectives

ForgeSync should provide:

1.  Multi-node Forgejo synchronisation.
2.  Repository-level primary designation.
3.  Git repository replication.
4.  Synchronisation of supported Forgejo metadata.
5.  User and identity mapping.
6.  Node discovery and health monitoring.
7.  Last-seen information and availability history.
8.  Replication status and lag monitoring.
9.  Conflict detection and controlled resolution.
10. New-node bootstrap.
11. ForgeSync high availability.
12. Secure administration through a web frontend.
13. An API and CLI for automation.
14. Auditing of administrative and synchronisation actions.
15. No dependency on Forgejo source-code modifications.
16. Graceful degradation if ForgeSync itself becomes unavailable.

------------------------------------------------------------------------

## 3. Non-objectives

ForgeSync should not:

-   modify the Forgejo database directly;
-   require identical Forgejo database IDs;
-   replicate Forgejo password hashes;
-   replace SceneID as the identity provider;
-   make Forgejo unusable without ForgeSync;
-   require a shared Forgejo database;
-   depend on blind bidirectional `git push --mirror`;
-   silently overwrite divergent Git history;
-   silently discard conflicting metadata changes;
-   require every Forgejo node to share the same filesystem.

------------------------------------------------------------------------

## 4. Core design principles

### 4.1 Forgejo remains independent

Every node must remain a valid standard Forgejo installation.

If ForgeSync is permanently removed, each Forgejo node should still be
usable with the data that has already been synchronised to it.

### 4.2 No direct Forgejo database manipulation

ForgeSync should interact through supported interfaces:

-   Forgejo REST API;
-   Forgejo webhooks;
-   Git SSH/HTTPS protocols;
-   Git LFS protocol/API;
-   supported package/release APIs where applicable.

Direct SQL writes into Forgejo databases must not be used.

### 4.3 Global ForgeSync IDs

Forgejo-local numeric IDs must never be assumed to match.

ForgeSync assigns global identifiers and maintains mappings:

``` text
Global user UUID
  SceneID subject: abc123
  SE Forgejo user: 152
  DK Forgejo user: 152
  DE Forgejo user: 187
```

The same principle applies to repositories, organisations and other
objects.

### 4.4 Repository-level primary

Primary ownership is defined per repository rather than globally per
Forgejo server.

``` text
Repository       Primary       Replicas
demo             SE            DK, DE
music            DK            SE, DE
tools            DE            SE, DK
```

Primary means authoritative when state is ambiguous. It does not
inherently mean that replica nodes are unavailable to users.

### 4.5 Eventual consistency with verification

Webhooks provide fast change detection, while periodic reconciliation
verifies that events were not missed.

### 4.6 Safe failure

A ForgeSync outage must not automatically take Forgejo offline.

------------------------------------------------------------------------

## 5. Forgejo-side prerequisites

The goal is **configuration only**, not modification.

### Required

Each Forgejo node needs:

1.  A supported Forgejo version.
2.  Network reachability from the appropriate ForgeSync
    controller/agent.
3.  An API service account or API token with sufficient privileges for
    the enabled synchronisation features.
4.  Git credentials for repository synchronisation.
5.  Webhooks pointing to ForgeSync, preferably created automatically by
    ForgeSync through the API.
6.  SceneID/OIDC configured consistently if common user identity is
    required.
7.  TLS certificates for normal HTTPS operation.

### Potentially required depending on enabled features

-   Git LFS access.
-   Package registry access.
-   Release asset access.
-   Administrative API permission for user/organisation provisioning.
-   SSH key/deploy-key permissions.
-   Actions API access.

### Not required

ForgeSync should require **none** of the following:

-   patched Forgejo binaries;
-   Forgejo source changes;
-   custom Forgejo plugins;
-   custom database tables inside Forgejo;
-   Forgejo database replication;
-   direct Forgejo SQL access;
-   custom Git hooks;
-   shared Forgejo filesystem;
-   shared user password database.

The exact API privileges should follow least privilege. A dedicated
ForgeSync service identity is preferable to using a human administrator
account.

------------------------------------------------------------------------

## 6. Identity and SceneID

SceneID should remain the authoritative authentication service.

``` text
                    SceneID
                       |
                    OIDC/OAuth
              +--------+--------+
              |        |        |
              v        v        v
          Forgejo SE Forgejo DK Forgejo DE
```

ForgeSync should use the stable SceneID/OIDC subject (`sub`) as the
canonical external identity wherever possible.

ForgeSync does **not** need to synchronise:

-   passwords;
-   password hashes;
-   MFA secrets belonging to SceneID;
-   SceneID authentication sessions.

ForgeSync may need to synchronise/map:

-   local Forgejo user records;
-   organisation membership;
-   teams;
-   repository permissions;
-   selected SSH keys;
-   selected application-specific authorisation state.

When a user first appears on one node, ForgeSync can provision/map the
corresponding account on other nodes where the Forgejo API supports this
safely.

Identical Forgejo numeric user IDs are desirable only as a convenience.
They are not a requirement.

------------------------------------------------------------------------

## 7. ForgeSync components

### 7.1 Controller

Recommended language: **Go**.

Responsibilities:

-   cluster state;
-   repository-primary ownership;
-   scheduling;
-   event processing;
-   identity/object mapping;
-   reconciliation;
-   conflict management;
-   health decisions;
-   node bootstrap;
-   auditing;
-   administrative API;
-   leadership management.

### 7.2 Agent

Recommended language: **Go**.

An agent can be deployed close to each Forgejo node.

Responsibilities:

-   local Forgejo API interaction;
-   Git operations;
-   LFS transfers;
-   state hashing;
-   local health checks;
-   webhook ingestion/forwarding;
-   direct node-to-node transfers when instructed.

Agents should not make global primary/failover decisions independently.

### 7.3 Web frontend

Recommended stack:

-   React;
-   TypeScript;
-   Vite.

The frontend is an administrative presentation layer only. It must never
contain Forgejo administrative credentials, replication keys, database
passwords or cluster secrets.

### 7.4 CLI

Recommended: Go with Cobra or an equivalent lightweight CLI framework.

Example future commands:

``` text
forgesync node list
forgesync node add
forgesync repo status
forgesync repo set-primary
forgesync sync retry
forgesync conflict list
```

### 7.5 Database

Recommended: PostgreSQL.

ForgeSync stores control-plane state, not a replacement copy of the
complete Forgejo databases.

Expected data includes:

-   nodes;
-   node capabilities;
-   identities;
-   organisations;
-   repositories;
-   object mappings;
-   repository primaries;
-   event journal;
-   replication jobs;
-   state hashes;
-   sync checkpoints;
-   conflicts;
-   leadership leases;
-   health history;
-   audit log.

------------------------------------------------------------------------

## 8. Suggested repository layout

``` text
forgesync/
├── cmd/
│   ├── forgesync/
│   ├── forgesync-agent/
│   └── forgesync-cli/
├── internal/
│   ├── api/
│   ├── auth/
│   ├── audit/
│   ├── cluster/
│   ├── conflict/
│   ├── forgejo/
│   ├── git/
│   ├── health/
│   ├── identity/
│   ├── reconcile/
│   ├── replication/
│   └── scheduler/
├── web/
├── migrations/
├── docs/
└── tests/
```

The frontend can be compiled and embedded into the Go controller binary
for simple deployment while remaining logically separated from the
backend.

------------------------------------------------------------------------

## 9. Security architecture

### 9.1 Browser access

``` text
Browser
   |
 HTTPS
   v
Reverse proxy / ForgeSync Web
   |
 authenticated API
   v
ForgeSync Controller
```

Human authentication should preferably use SceneID through OIDC.

Suggested roles:

-   Viewer;
-   Operator;
-   Administrator.

### 9.2 Browser restrictions

The browser must never receive:

-   Forgejo API tokens;
-   Forgejo administrator credentials;
-   SSH private keys;
-   agent credentials;
-   database credentials;
-   SceneID client secrets;
-   ForgeSync cluster secrets.

### 9.3 Controller-to-agent security

Use mutual TLS (mTLS).

Every agent should have an individual identity/certificate.

``` text
Controller
    |
   mTLS
 +--+--+
 |  |  |
 SE DK DE
```

Compromise of one agent must not allow it to impersonate another.

### 9.4 API separation

Suggested logical interfaces:

``` text
/api/v1/          Human/browser administration
/internal/v1/     Controller-to-controller
/agent/v1/        Controller-to-agent
```

Only the required public API should be exposed through the public
reverse proxy.

### 9.5 Secrets

Secrets should be encrypted at rest.

A master encryption key must not be stored in the same PostgreSQL
database as encrypted credentials.

For example:

``` text
PostgreSQL:
  encrypted Forgejo token

/etc/forgesync/master.key:
  root/ForgeSync protected master key
```

An HA pair must have a secure method of obtaining the required
encryption key.

### 9.6 Audit

Sensitive actions should be auditable, including:

-   node addition/removal;
-   repository-primary changes;
-   conflict resolution;
-   credential changes;
-   manual synchronisation;
-   failover/promotion;
-   configuration changes.

------------------------------------------------------------------------

## 10. ForgeSync high availability

ForgeSync can initially run as a single controller.

Production deployments should support:

``` text
ForgeSync SE       ForgeSync DK
LEADER             FOLLOWER
     \               /
      \             /
       PostgreSQL HA
```

Only one controller should execute authoritative control-plane
operations at a time.

### Terminology

Three different concepts must remain distinct:

1.  **ForgeSync Leader** --- controller currently controlling
    replication.
2.  **Repository Primary** --- authoritative Forgejo node for a
    particular repository.
3.  **Database Primary** --- PostgreSQL primary for ForgeSync state.

They need not be located on the same physical site.

### Split-brain protection

A two-controller installation requires fencing/leader-election
protection.

For automatic failover, an independent witness/quorum mechanism is
recommended:

``` text
            Witness
           /       \
ForgeSync SE ----- ForgeSync DK
```

A controller should require quorum/lease ownership before assuming
leadership.

A simpler initial release can use manual ForgeSync failover rather than
unsafe automatic promotion.

------------------------------------------------------------------------

## 11. DNS-like control-plane role

ForgeSync should know:

-   every Forgejo node;
-   hostname;
-   site/location;
-   online/offline state;
-   last seen;
-   capabilities;
-   current load/health;
-   repository availability;
-   repository primary;
-   replication status;
-   replication lag.

This makes ForgeSync conceptually similar to a service directory/DNS
control plane.

However, ForgeSync should not initially implement an authoritative DNS
server.

A safer separation is:

``` text
ForgeSync
   |
   | health/role information
   v
HAProxy / DNS automation
   |
   v
Forgejo nodes
```

Later, optional integration could update DNS records, HAProxy backends,
service discovery, or other routing systems.

------------------------------------------------------------------------

## 12. Node health monitoring

A node is not considered healthy merely because TCP/HTTPS responds.

Checks can include:

-   HTTPS reachability;
-   Forgejo API response;
-   API authentication;
-   Git HTTPS;
-   Git SSH;
-   LFS endpoint;
-   webhook activity;
-   replication responsiveness.

Suggested states:

``` text
HEALTHY
DEGRADED
SUSPECT
UNREACHABLE
AUTH_ERROR
SYNC_LAGGING
MAINTENANCE
UNKNOWN
```

Store:

-   last health check;
-   last successful contact;
-   failure start;
-   recovery time;
-   historical uptime;
-   current replication lag.

Example:

``` text
Forgejo DK

Status:            UNREACHABLE
Last checked:      09:48:12
Last seen:         09:44:37
Unavailable for:   3m 35s
```

------------------------------------------------------------------------

## 13. Synchronisation model

### Fast path

Webhooks report changes quickly:

``` text
User -> Forgejo SE -> webhook -> ForgeSync -> replicate -> Forgejo DK
```

### Verification path

Periodic reconciliation checks actual state:

``` text
ForgeSync
  |
  +-- inspect SE
  +-- inspect DK
  +-- inspect DE
  |
  +-- compare state/hashes
  |
  +-- repair or flag differences
```

Webhooks should therefore be an optimisation, not the only source of
truth.

------------------------------------------------------------------------

## 14. Loop prevention

A replicated operation can itself generate a webhook. ForgeSync must
recognise its own replicated changes.

Maintain information such as:

``` text
event_uuid
origin_node
global_object_uuid
object_version
payload_hash
timestamp
```

Previously processed or self-generated events can then be suppressed
safely.

------------------------------------------------------------------------

## 15. Git replication

Git should be synchronised through normal Git protocols rather than by
modifying Forgejo storage directly.

For every relevant change:

1.  fetch source refs;
2.  inspect destination refs;
3.  determine ancestry;
4.  replicate safe fast-forward/new-ref changes;
5.  detect divergence;
6.  never silently force over divergent history;
7.  record a conflict when automatic resolution is unsafe.

Blind bidirectional `git push --mirror` should not form the basis of
ForgeSync.

------------------------------------------------------------------------

## 16. Forgejo metadata

ForgeSync should progressively support:

  Object                      Expected mechanism   Complexity
  --------------------------- -------------------- -------------
  Git commits/branches/tags   Git                  Low
  Wiki                        Git/API              Low
  Labels                      API                  Low
  Milestones                  API                  Low
  Releases                    API                  Low/Medium
  Release assets              API/files            Medium
  Issues                      API                  Medium
  Comments                    API                  Medium
  LFS                         LFS/API              Medium
  Repository settings         API                  Medium
  Collaborators               API                  Medium
  Teams                       API                  Medium
  Pull requests               API + Git            High
  Reviews                     API                  High
  Actions                     API                  High
  Actions secrets             Restricted/special   Very high
  Packages                    API/protocol         Medium/High

Before implementation, a version-specific Forgejo API coverage matrix
should confirm exactly which fields can be read, created and updated
through supported APIs.

------------------------------------------------------------------------

## 17. Conflict handling

ForgeSync must distinguish non-conflicting concurrent changes from
genuine conflicts.

Example:

``` text
SE: issue status changed
DK: issue title changed
```

These may be mergeable.

But:

``` text
SE: title = "Server crash"
DK: title = "Network crash"
```

requires conflict policy.

The repository primary provides the default authoritative state, but the
losing change should not simply disappear. It should be recorded in the
conflict/audit history and, where appropriate, presented for
administrative resolution.

Git divergence must be treated particularly conservatively.

------------------------------------------------------------------------

## 18. Object versioning

ForgeSync should maintain its own logical versions/checkpoints rather
than relying solely on timestamps.

Conceptually:

``` text
Object UUID: abc123

SE version: 48
DK version: 48
DE version: 48
```

Different state hashes at concurrent versions indicate divergence
requiring reconciliation.

------------------------------------------------------------------------

## 19. Adding additional Forgejo nodes

Adding node three, four or ten should use the same process.

Suggested lifecycle:

``` text
NEW
 |
REGISTERED
 |
BOOTSTRAPPING
 |
VERIFYING
 |
SYNCED
 |
ACTIVE
```

During bootstrap, the node should not normally accept normal writes.

Bootstrap should:

1.  verify Forgejo/API version;
2.  verify credentials;
3.  verify Git;
4.  verify SceneID configuration;
5.  determine capabilities;
6.  install/register required webhooks;
7.  map/provision identities;
8.  synchronise organisations/teams;
9.  create repositories;
10. synchronise Git;
11. synchronise LFS/assets;
12. synchronise supported metadata;
13. calculate/compare state;
14. mark the node active only after verification.

------------------------------------------------------------------------

## 20. Avoid full-mesh replication

Do not configure every Forgejo server to blindly mirror every other
server.

ForgeSync should maintain topology centrally.

``` text
                    ForgeSync
                 /      |      \
                /       |       \
               SE      DK       DE
```

Large transfers can still occur directly between nodes/agents.

For example:

``` text
ForgeSync instructs:
Agent DK ==================> Agent DE
            Git/LFS data
```

The controller coordinates the transfer without becoming the bulk-data
path.

------------------------------------------------------------------------

## 21. Failure behaviour

### Forgejo node failure

Suggested progression:

``` text
HEALTHY
  |
SUSPECT
  |
UNREACHABLE
  |
FAILURE CONFIRMED
  |
primary evaluation
  |
optional promotion
```

Short network interruptions must not immediately cause promotion.

### ForgeSync failure

If no ForgeSync leader exists, Forgejo nodes should continue to operate.

What stops:

-   replication;
-   reconciliation;
-   automatic primary changes;
-   automated failover.

A recommended safety policy is:

``` yaml
cluster:
  no_leader_policy: primary_only
```

Repository primaries remain writable while replicas can be treated as
read-only or otherwise restricted by policy until coordination returns.

Implementing strict write restriction without Forgejo modifications may
require routing/proxy controls rather than modifying Forgejo itself;
this must be validated during implementation.

### Recovery

A returning node should:

``` text
RECOVERING
   |
catch up
   |
VERIFYING
   |
SYNCED
   |
REPLICA/ACTIVE
```

Automatic failback to the old primary is not recommended.

------------------------------------------------------------------------

## 22. Frontend requirements

The administrative frontend should provide at least:

### Dashboard

-   ForgeSync leader/follower state;
-   database state;
-   Forgejo node health;
-   last seen;
-   replication health;
-   repository counts;
-   conflicts;
-   current incidents.

### Nodes

For each node:

-   name;
-   URL;
-   site/location;
-   Forgejo version;
-   status;
-   last seen;
-   API status;
-   Git status;
-   LFS status;
-   capabilities;
-   current sync workload.

### Repositories

For each repository:

-   global identity;
-   participating nodes;
-   primary;
-   sync state;
-   last successful sync;
-   replication lag;
-   Git state;
-   metadata state;
-   conflicts.

### Conflicts

-   object;
-   nodes involved;
-   detected time;
-   primary state;
-   alternate state;
-   resolution;
-   operator;
-   audit trail.

### Events/Audit

Searchable historical event and administrative log.

------------------------------------------------------------------------

## 23. Suggested technology stack

  Area                              Recommendation
  --------------------------------- ------------------------------------
  Controller                        Go
  Agent                             Go
  CLI                               Go
  Database                          PostgreSQL
  PostgreSQL driver                 pgx
  HTTP router                       chi
  Structured logging                slog
  Frontend                          React
  Frontend language                 TypeScript
  Frontend build                    Vite
  Human authentication              SceneID/OIDC
  Browser API                       REST + SSE
  Agent/controller authentication   mTLS
  Metrics                           Prometheus format
  Tracing                           OpenTelemetry
  Git                               Native Git initially
  Service management                systemd
  Packaging                         Native binary + optional container
  Reverse proxy                     HAProxy/Nginx or equivalent

Avoid unnecessary infrastructure in the initial design, including
mandatory Kubernetes, Kafka or a large microservice architecture.

------------------------------------------------------------------------

## 24. Installation models

### Small installation

``` text
Server
├── ForgeSync Controller
├── embedded ForgeSync Web UI
└── PostgreSQL
```

Agents may initially be incorporated into the controller where network
topology allows it.

### Production HA

``` text
SITE SE                              SITE DK

ForgeSync Controller                ForgeSync Controller
LEADER                              FOLLOWER
       \                            /
        \------ PostgreSQL HA -----/
                  |
               Witness
                  |
        +---------+---------+
        |         |         |
     Agent SE  Agent DK  Agent DE
        |         |         |
     Forgejo   Forgejo   Forgejo
```

------------------------------------------------------------------------

## 25. Minimum infrastructure prerequisites

### ForgeSync Controller

Suggested initial baseline per controller:

-   Linux;
-   systemd or container runtime;
-   Go binary/runtime-independent compiled executable;
-   TLS certificate;
-   network connectivity to PostgreSQL and agents;
-   SceneID client registration;
-   protected secret/key storage.

### PostgreSQL

-   supported PostgreSQL release;
-   backups;
-   TLS where crossing untrusted networks;
-   replication for HA deployment;
-   tested recovery process.

### Agent

-   Linux;
-   ForgeSync agent binary;
-   Git client;
-   network access to local/remote Forgejo as required;
-   mTLS certificate/private key;
-   appropriate Forgejo API and Git credentials.

### Networking

Required flows should be explicitly documented and restricted by
firewall:

-   browser -\> ForgeSync HTTPS;
-   Forgejo -\> ForgeSync webhook HTTPS;
-   controller -\> PostgreSQL;
-   controller \<-\> controller;
-   controller \<-\> agent;
-   agent -\> Forgejo API;
-   agent -\> Forgejo Git SSH/HTTPS;
-   agent \<-\> agent where direct replication is enabled;
-   Forgejo/agent -\> SceneID as required.

------------------------------------------------------------------------

## 26. Recommended implementation phases

### Phase 0 --- Compatibility study

Produce a Forgejo API coverage matrix for supported Forgejo releases.

### Phase 1 --- Core/MVP

Implement:

-   node registration;
-   Forgejo discovery;
-   health monitoring;
-   last seen;
-   repository discovery;
-   global object IDs;
-   primary designation;
-   Git replication;
-   webhook ingestion;
-   loop prevention;
-   divergence detection;
-   periodic reconciliation;
-   basic dashboard;
-   audit log.

### Phase 2 --- Metadata

Add:

-   labels;
-   milestones;
-   issues;
-   comments;
-   releases;
-   assets;
-   wiki;
-   LFS;
-   repository settings.

### Phase 3 --- Collaboration

Add:

-   organisations;
-   teams;
-   permissions;
-   user provisioning/mapping;
-   pull requests;
-   reviews;
-   packages;
-   selected Actions functionality.

### Phase 4 --- HA control plane

Add:

-   secondary ForgeSync controller;
-   leader election;
-   PostgreSQL HA;
-   witness/quorum;
-   safe controller failover;
-   repository-primary promotion;
-   recovery workflows;
-   optional routing/DNS integration.

------------------------------------------------------------------------

## 27. Initial state machine

Repository/node synchronisation should use explicit states, for example:

``` text
SYNCED
  |
BEHIND
  |
SYNCING
  |
VERIFYING
  |
SYNCED
```

Exceptional states:

``` text
DIVERGED
CONFLICT
UNREACHABLE
AUTH_ERROR
PAUSED
MAINTENANCE
```

State transitions should be persisted and auditable.

------------------------------------------------------------------------

## 28. Important open design questions

Before coding beyond the MVP, decide:

1.  Which Forgejo versions are supported?
2.  Which metadata objects are guaranteed to replicate?
3.  How are writes to replica nodes handled when the primary is
    reachable?
4.  How are replica writes restricted when ForgeSync has no leader
    without modifying Forgejo?
5.  Which user attributes are provisioned versus left to SceneID/Forgejo
    login?
6.  Are SSH keys synchronised?
7.  Are personal access tokens ever synchronised? The recommended
    default is **no**.
8.  How are deploy keys handled?
9.  How are Actions secrets handled? The recommended default is **do not
    attempt generic replication initially**.
10. How is LFS replicated and verified?
11. What constitutes repository consistency?
12. What is the automatic failover threshold?
13. Is repository-primary promotion manual in v1?
14. Which DNS/load-balancer systems may consume ForgeSync health
    information?
15. What is the retention period for events, health history and audit
    data?

------------------------------------------------------------------------

## 29. Recommended security defaults

ForgeSync should ship with conservative defaults:

-   TLS required;
-   OIDC required for web administration where configured;
-   mTLS required for agents;
-   least-privilege Forgejo service accounts;
-   no direct Forgejo SQL access;
-   encrypted stored secrets;
-   audit logging enabled;
-   automatic destructive conflict resolution disabled;
-   automatic force-push disabled;
-   automatic failback disabled;
-   repository promotion manual until quorum/fencing is configured;
-   internal APIs not exposed publicly;
-   secure HTTP headers;
-   CSRF protection for browser operations;
-   short-lived sessions;
-   rate limiting on authentication and sensitive APIs.

------------------------------------------------------------------------

## 30. Summary architecture

``` text
                               SceneID
                                  |
                              OIDC/OAuth
                                  |
                                  v
                        sync.scenegit.org
                                  |
                           Reverse Proxy
                                  |
                    +-------------+-------------+
                    |                           |
                    v                           v
             ForgeSync SE                ForgeSync DK
                LEADER                     FOLLOWER
          Go + embedded Web UI        Go + embedded Web UI
                    |                           |
                    +-------------+-------------+
                                  |
                           PostgreSQL HA
                                  |
                             Witness/Quorum
                                  |
             +--------------------+--------------------+
             |                    |                    |
            mTLS                 mTLS                 mTLS
             |                    |                    |
             v                    v                    v
       ForgeSync Agent      ForgeSync Agent      ForgeSync Agent
             |                    |                    |
             v                    v                    v
        Forgejo SE           Forgejo DK           Forgejo DE
          stock                stock                stock
             \                    |                    /
              \________________ SceneID ______________/
```

The architecture deliberately keeps ForgeSync outside Forgejo.

**The expected Forgejo-side change is configuration and credentials, not
software modification:** an appropriate ForgeSync service/API identity,
Git access, webhook configuration, and the already-used SceneID/OIDC
integration. Exact permissions and any feature-specific API limitations
must be validated against the supported Forgejo versions before
implementation.

ForgeSync then becomes the distributed control plane: it knows which
Forgejo nodes exist, which are healthy, when they were last seen, which
repository is primary on which node, whether replicas are current, and
what needs to be synchronised. An optional secondary ForgeSync
controller removes the control-plane single point of failure, while a
witness/quorum mechanism makes automatic leadership changes safe.

The key operational property remains: **Forgejo must continue to be
Forgejo.** ForgeSync coordinates and synchronises it but does not make a
Forgejo installation dependent on custom ForgeSync modifications.
