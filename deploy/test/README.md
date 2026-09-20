# ForgeSync test environment

A local test environment for Phase 0 (checking what stock Forgejo supports) and for
developing ForgeSync, running on Docker Desktop for macOS.

| Service | URL | Purpose |
|---|---|---|
| `sceneid` | http://sceneid.test:8080 | Keycloak, standing in for SceneID (OIDC) |
| `forgejo-se` | http://forgejo-se.test:3001 (SSH port 2221) | Forgejo node SE |
| `forgejo-dk` | http://forgejo-dk.test:3002 (SSH port 2222) | Forgejo node DK |
| `forgejo-de` | http://forgejo-de.test:3003 (SSH port 2223) | Forgejo node DE (optional: `--three` or `--all`) |
| `forgejo-uk` | http://forgejo-uk.test:3004 (SSH port 2224) | Forgejo node UK (optional: `--all`) |
| `forgejo-us` | http://forgejo-us.test:3005 (SSH port 2225) | Forgejo node US (optional: `--all`) |
| `forgesync-db` | localhost:5432 | PostgreSQL for ForgeSync's own state (user/db `forgesync`) |
| `hooksink` | http://localhost:8099 | Records webhook deliveries for the Phase 0 tests |
| `forgesync` | http://forgesync.test:8090 | The ForgeSync controller and admin UI, built from this repository |
| `forgesync-b` | http://forgesync-b.test:8091 | A second controller sharing the database, on standby (`setup.sh --standby`) |

This is the environment for trying things and for developing. To run ForgeSync properly,
natively on Debian, see [../prod/README.md](../prod/README.md).

Versions are pinned in `.env`: Forgejo 16 (current LTS) and Keycloak 26.7.4. Both images
run natively on Apple Silicon.

## Prerequisites

- Docker Desktop for macOS, with at least 4 GB of memory allocated. Keycloak alone uses about 1 GB.
- Add the hostnames once:

  ```sh
  echo '127.0.0.1 sceneid.test forgesync.test forgejo-se.test forgejo-dk.test forgejo-de.test forgejo-uk.test forgejo-us.test' | sudo tee -a /etc/hosts
  ```

  Every service is reached by the same URL from the Mac and from inside other containers.
  OIDC needs this, because the issuer URL the browser sees must match the one Forgejo sees.
  Mirroring between nodes needs it too.

## Running on a server

The same setup runs on a Linux server with Docker. On the server:

```sh
echo '127.0.0.1 sceneid.test forgesync.test forgejo-se.test forgejo-dk.test forgejo-de.test forgejo-uk.test forgejo-us.test' | sudo tee -a /etc/hosts
PUBLIC_BIND=0.0.0.0 ./setup.sh --all
```

`PUBLIC_BIND=0.0.0.0` publishes SceneID (8080), Forgejo (3001-3003, SSH 2221-2223) and the
ForgeSync UI (8090) on the server's network interfaces. The database and webhook sink stay on
localhost. On each machine whose browser should use it, point the names at the server:

```sh
echo '<server-ip> sceneid.test forgesync.test forgejo-se.test forgejo-dk.test forgejo-de.test forgejo-uk.test forgejo-us.test' | sudo tee -a /etc/hosts
```

The names must be the same everywhere, because SceneID's issuer URL and the redirect URLs are
checked exactly. Pass `PUBLIC_BIND=0.0.0.0` to every later `docker compose ... up` as well:
a container recreated without it is published on localhost only. The test credentials are public (they're in this repository), so only do this on
a network you trust.

### Or address it by one host or IP, with no hosts file at all

```sh
PUBLIC_BIND=0.0.0.0 PUBLIC_HOST=10.0.0.5 ./setup.sh --all --standby
```

`PUBLIC_HOST` makes everything a browser sees use that name instead of the `*.test` ones:
Keycloak's hostname, the nodes' `ROOT_URL` and their SceneID login sources, both
controllers' issuer, redirect and webhook URLs, and the callbacks registered with the realm.
It has to be one name everywhere, since OIDC checks the issuer in the token against the one
the controller was configured with, and the `*.test` aliases keep working inside the compose
network, which is what container-to-container traffic uses either way. Pass the same
`PUBLIC_HOST` to every later `docker compose ... up` and to the scripts below.

## The other scripts

```sh
./check-dismissal.sh           # does conflict dismissal work, end to end?
./scale.sh make 200            # 200 repositories on SE, to measure a round
./scale.sh drop                # and away again
```

`check-dismissal.sh` makes a conflict ForgeSync can't settle (an Actions secret on one
node, which it may not read let alone copy), dismisses it, and checks the four things that
matter: the dashboard stops counting it, a fresh round leaves it dismissed, it comes back
when what it says changes, and it clears when the difference goes away. It cleans up after
itself, picks a repository whose Actions are on, and takes a few minutes because it waits
for five scan rounds.

`scale.sh` is for finding out what a round costs at a size you care about; the numbers from
203 repositories on five nodes are in `docs/PERFORMANCE.md`. Deleting
them exercises the archive flow on every other node, which is worth watching once.

Both take `PUBLIC_HOST=<host or IP>` when the environment isn't on the `*.test` names.

## Usage

```sh
./setup.sh            # start SE + DK, configure them, run smoke tests
./setup.sh --three    # same, including DE
./setup.sh --all      # SE, DK, DE, UK and US
docker compose logs -f forgejo-se
docker compose --profile three down -v   # stop everything and delete all data
                                         # (then rm -rf .tokens before the next setup)
```

`setup.sh` is safe to re-run. On each node it creates:

- the local admin accounts `siteadmin` (for people) and `forgesync` (the service account).
  These are the only local accounts, as the identity rule requires.
- the `SceneID` login source.
- an API token for `forgesync`, saved in `.tokens/<node>.token`, which git ignores.

## Identity configuration (applies to every node)

| Setting | Value | Why |
|---|---|---|
| `service.ALLOW_ONLY_EXTERNAL_REGISTRATION` | `true` | Blocks local sign-up; users are created only by SceneID login |
| `service.DISABLE_REGISTRATION` | `false` | Must stay `false`, or SceneID sign-up is blocked too |
| `openid.ENABLE_OPENID_SIGNIN` / `SIGNUP` | `false` | Turns off legacy OpenID 2.0 login, another external route |
| `oauth2_client.ENABLE_AUTO_REGISTRATION` | `true` | First SceneID login creates the account |
| `oauth2_client.USERNAME` | `nickname` | Forgejo has no `preferred_username` option; the realm maps the Keycloak username to a `nickname` claim |
| `oauth2_client.ACCOUNT_LINKING` | `auto` | A user ForgeSync creates in advance is linked on their first login instead of being duplicated |

The password login form stays on, because the local admins need it.

Test-only settings: `webhook.ALLOWED_HOST_LIST=*` and `migrations.ALLOW_LOCALNETWORKS=true`.
They let webhooks and mirrors reach local addresses. Don't copy them to production.

## SceneID stand-in (`sceneid/realm-sceneid.json`)

- Realm `sceneid`, with users `alice`, `bob` and `carol` (password `<name>-pw`, email verified).
- Client `forgejo` for all nodes, with secret `forgejo-test-secret`.
- Client `forgesync`: a service account allowed to view and manage users (`view-users`,
  `manage-users`), for the deprovisioning tests.
- Client `forgesync-admin`: SceneID sign-in for the ForgeSync admin UI. Realm roles
  `forgesync-admin`, `forgesync-operator` and `forgesync-viewer` go into a `roles` ID token
  claim.

Keycloak keeps no volume, so recreating the container resets SceneID to the JSON file. To apply
edits: `docker compose up -d --force-recreate sceneid`. This also discards any users or changes
made in the admin console.

## The ForgeSync controller

`setup.sh` builds the controller image from this repository and starts it as the `forgesync`
service. Its config is `forgesync.docker.yaml` plus a `nodes:` list for the nodes that setup
started, written to `.work/forgesync.docker.yaml`. Rerun `setup.sh` with the same option
after changing the set of nodes. After changing the code, rebuild and restart it:

```sh
PUBLIC_BIND=0.0.0.0 docker compose --profile controller up -d --build forgesync   # PUBLIC_BIND only on a server
docker compose --profile controller logs -f forgesync
```

To run it from source instead (e.g. with `make web-dev` for hot reload), stop the container first
(`docker compose --profile controller stop forgesync`), since both use port 8090. Then, from the repo
root, `make web && make run` uses `forgesync.yaml` and serves the UI at http://127.0.0.1:8090.

### A second controller, and failover

`setup.sh --standby` also starts `forgesync-b` on :8091, sharing the same database. Only one
controller acts: it holds the leadership lease in the database and renews it, and the other
stands by. Its config is `.work/forgesync-b.docker.yaml`, derived from the first
controller's with its own name, port and URLs. The lease here is 10s (production defaults to
15s), so a failover is quick to watch.

```sh
PUBLIC_BIND=0.0.0.0 docker compose --profile controller --profile standby up -d --build
curl -s -H "Authorization: Bearer $(cat .tokens/admin.token)" http://127.0.0.1:8090/api/v1/overview | jq .role
curl -s -H "Authorization: Bearer $(cat .tokens/admin.token)" http://127.0.0.1:8091/api/v1/overview | jq .role
```

The standby serves every page, so you can watch what's happening, but turns away anything
that changes the installation with 409 and the name of the controller in charge. To see a
failover, stop the leader:

```sh
docker kill forgesync-test-forgesync-b-1       # a crash: the standby takes over when the lease runs out
docker stop forgesync-test-forgesync-b-1       # a planned stop: it gives the lease up, so in about one renewal
```

The new leader reinstalls each node's webhook to point at itself, scans, and carries on. Add
`forgesync-b.test` to `/etc/hosts` beside the other names to reach its UI by name.

The CLI works against either:

```sh
go run ./cmd/forgesync --server http://forgesync.test:8090 --token-file deploy/test/.tokens/admin.token node list
```

The admin UI is at http://forgesync.test:8090. Sign in with SceneID as one of the test users:

| User | ForgeSync role |
|---|---|
| `alice` / `alice-pw` | Administrator |
| `bob` / `bob-pw` | Operator |
| `carol` / `carol-pw` | Viewer (no audit log) |
| `erin` / `erin-pw` | none, so sign-in is refused |

"Use the admin token instead" (the contents of `.tokens/admin.token`) is kept as a break-glass
option in this config. If your `sceneid` container was created before the `forgesync-admin`
client was added to the realm, recreate it: `docker compose up -d --force-recreate sceneid`.
To work on the UI with hot reload, run `make web-dev` alongside `make run` and open
http://127.0.0.1:5173.

Containers reach a ForgeSync process running on the Mac through `http://host.docker.internal:<port>`.
Use that address as the webhook target.

## Webhook sink

The `hooksink` service records the webhooks it receives, for the Phase 0 tests (`../../phase0`).
Forgejo sends webhooks to `http://hooksink:8099/hook/<tag>`, and the tests read them back from
`http://localhost:8099/events?after=<seq>`. The sink checks each delivery's HMAC signature
against `HOOK_SECRET`.

## Replication

`forgesync.yaml` has replication on. It copies branches and tags from a repository's primary
node to the others, for repositories whose primary is set on the Repositories page or with
`forgesync repo set-primary`. It uses `git` on the Mac (2.32 or later; Xcode's is fine),
with a cache in `.work/git`.

It only replicates into repositories that already exist on the other node. Create the
repository there first; automatic creation waits on the Phase 0 identity results. Commits
made directly on a replica are never overwritten: they show up as conflicts.
