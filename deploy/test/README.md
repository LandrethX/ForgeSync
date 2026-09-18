# ForgeSync test environment

A local test environment for Phase 0 (checking what stock Forgejo supports) and for
developing ForgeSync, running on Docker Desktop for macOS.

| Service | URL | Purpose |
|---|---|---|
| `sceneid` | http://sceneid.test:8080 | Keycloak, standing in for SceneID (OIDC) |
| `forgejo-se` | http://forgejo-se.test:3001 (SSH port 2221) | Forgejo node SE |
| `forgejo-dk` | http://forgejo-dk.test:3002 (SSH port 2222) | Forgejo node DK |
| `forgejo-de` | http://forgejo-de.test:3003 (SSH port 2223) | Forgejo node DE (optional, `--three`) |
| `forgesync-db` | localhost:5432 | PostgreSQL for ForgeSync's own state (user/db `forgesync`) |
| `hooksink` | http://localhost:8099 | Records webhook deliveries for the Phase 0 tests |

Versions are pinned in `.env`: Forgejo 16 (current LTS) and Keycloak 26.7.4. Both images
run natively on Apple Silicon.

## Prerequisites

- Docker Desktop for macOS, with at least 4 GB of memory allocated. Keycloak alone uses about 1 GB.
- Add the hostnames once:

  ```sh
  echo '127.0.0.1 sceneid.test forgejo-se.test forgejo-dk.test forgejo-de.test' | sudo tee -a /etc/hosts
  ```

  Every service is reached by the same URL from the Mac and from inside other containers.
  OIDC needs this, because the issuer URL the browser sees must match the one Forgejo sees.
  Mirroring between nodes needs it too.

## Usage

```sh
./setup.sh            # start SE + DK, configure them, run smoke tests
./setup.sh --three    # same, including DE
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

Keycloak keeps no volume, so recreating the container resets SceneID to the JSON file. To apply
edits: `docker compose up -d --force-recreate sceneid`. This also discards any users or changes
made in the admin console.

## ForgeSync on the Mac

Run the controller on the Mac from the repo root. It uses `forgesync.yaml`, the tokens
`setup.sh` created in `.tokens/`, and the `forgesync-db` database:

```sh
make web && make run                                       # builds the UI, then runs the controller
go run ./cmd/forgesync --token-file deploy/test/.tokens/admin.token node list
```

The admin UI is at http://127.0.0.1:8090. Sign in with the contents of `.tokens/admin.token`.
To work on the UI with hot reload, run `make web-dev` alongside `make run` and open
http://127.0.0.1:5173.

Containers reach a ForgeSync process running on the Mac through `http://host.docker.internal:<port>`.
Use that address as the webhook target.

## Webhook sink

The `hooksink` service records the webhooks it receives, for the Phase 0 tests (`../../phase0`).
Forgejo sends webhooks to `http://hooksink:8099/hook/<tag>`, and the tests read them back from
`http://localhost:8099/events?after=<seq>`. The sink checks each delivery's HMAC signature
against `HOOK_SECRET`.
