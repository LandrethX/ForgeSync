# Running ForgeSync in production

Two controllers, one PostgreSQL database, and the Forgejo nodes they look
after. One controller does the work; the other serves the same pages and
takes over when it stops. Everything here has been run — the numbers and
the restore below come from doing it, not from reasoning about it.

| | |
|---|---|
| `forgesync.yaml` | a production config, annotated; copy and change what's marked |
| `forgesyncd.service` | a systemd unit, with the usual hardening |
| `backup.sh` | takes (and optionally verifies) a dump of the database |

## What runs where

- **Two controllers**, one per host, each with its own config. They differ
  in `controller.name`, `controller.url`, `controller.priority` and the
  webhook URL; everything else, including `database.url`, is the same.
  Sharing the database is what pairs them.
- **One PostgreSQL**, reachable from both. See *The database* below.
- **The Forgejo nodes**, each with a `forgesync` site-admin account whose
  token is in `nodes[].token_file`, and each with the controllers' hosts
  in `[webhook] ALLOWED_HOST_LIST` so their webhooks can reach whichever
  is leading.

Install, on each controller host:

```sh
useradd --system --home /var/lib/forgesync --shell /usr/sbin/nologin forgesync
install -d -o forgesync -g forgesync -m 0750 /var/lib/forgesync /etc/forgesync
install -d -o root -g forgesync -m 0750 /etc/forgesync/secrets
install -m 0755 bin/forgesyncd /usr/local/bin/forgesyncd
install -m 0640 -o root -g forgesync forgesync.yaml /etc/forgesync/forgesync.yaml
install -m 0644 forgesyncd.service /etc/systemd/system/forgesyncd.service
systemctl enable --now forgesyncd
```

Every secret is a file that ForgeSync reads at startup: the database URL,
the admin token, the webhook secret, and one API token per node. None of
them is ever taken from the config or the environment, so they don't end
up in a process list, a log or a core dump.

## Signing in

ForgeSync has accounts of its own, in the shared database, so they work
on either controller. The first one is made with the admin token, there
being nobody to make it otherwise:

```sh
curl -X PUT -H "Authorization: Bearer $(cat /etc/forgesync/secrets/admin.token)" \
     -H 'Content-Type: application/json' \
     -d '{"username":"you","password":"a long passphrase","role":"administrator"}' \
     https://forgesync-a.example.org/api/v1/accounts
```

After that, **ForgeSync accounts** in the UI. SceneID signs people in to
the *nodes*; it has nothing to do with the controllers.

## TLS

Either terminate TLS in front of it (leave the `tls_*` settings out and
let the proxy talk HTTP to `listen`), or let ForgeSync do it:
`http.tls_cert_file` and `http.tls_key_file`. With `http.tls_listen` it
does both at once — plain HTTP on `listen` for a proxy on the same host,
HTTPS on `tls_listen` for browsers reaching it directly.

`systemctl reload forgesyncd` re-reads the certificate in place, for a
renewal; a pair that won't load leaves the one in use rather than taking
the controller off the air.

## Watching it

`/metrics` (Prometheus text format, needs the Viewer role, so the scraper
sends the admin token as a bearer token). What to alert on, in order:

| Series | Why |
|---|---|
| `forgesync_leader` | exactly one controller should be acting. Zero means nobody is syncing, whatever the pages say |
| `forgesync_database_up` | a controller that has lost the database stops acting before its lease runs out |
| `forgesync_node_healthy` | a node that isn't answering isn't being replicated to |
| `forgesync_scan_last_success_timestamp_seconds` | a scan that stops is a sync that stops, and nothing else shows it |
| `forgesync_replicas{state}` | anything that isn't `synced` is waiting for a person or a retry |
| `forgesync_conflicts_open` | differences ForgeSync won't decide on its own |

The round logs its own duration; `log.level: debug` adds a line per part
(`scan round part finished`), which is how you find out what to change if
a round outgrows `inventory.interval`.

## Backups

The database is the only thing that can't be rebuilt. It holds what each
repository's primary is, what ForgeSync last wrote to every replica, the
merge bases behind every "one new value wins" decision, the conflicts
people are working through, the archived copies of deleted repositories,
ForgeSync's accounts and the audit log.

```sh
./backup.sh /var/backups/forgesync            # nightly, from cron or a timer
./backup.sh /var/backups/forgesync --verify   # weekly: restores it into a scratch database
```

Not in the backup, on purpose:

- **The git cache** under `replication.work_dir`. It is a cache; delete it
  and the next run refetches. Don't share it between controllers.
- **The Forgejo nodes.** Whoever runs them backs them up; ForgeSync is
  not a backup of them, and they are not a backup of it.
- **The secrets.** They belong wherever your secrets already live.

### Restoring

```sh
systemctl stop forgesyncd            # on BOTH controllers
psql "$URL_TO_postgres" -c 'DROP DATABASE forgesync WITH (FORCE)'
psql "$URL_TO_postgres" -c 'CREATE DATABASE forgesync'
pg_restore --dbname="$FORGESYNC_DATABASE_URL" --no-owner forgesync-....dump
systemctl start forgesyncd           # one controller first, then the other
```

Then check: the repository count is what it was, `forgesync_replicas` is
all `synced`, and no conflicts appeared that weren't there before.

Restoring over a running installation was tried on the test environment:
three repositories, twelve replicas and one open conflict before; the
same three, twelve and one after, accounts still signing in, no errors.

### If the database is lost with no backup

Nothing on the nodes is damaged, and ForgeSync rebuilds most of itself
from them: it rediscovers every repository, assigns primaries by the
rules (owner's site, then origin), and replication carries on. This was
tried too — from an empty database the test environment came back with
all replicas in sync and nothing overwritten.

What is gone is everything nobody can infer from the nodes:

- **every choice an administrator made**: a repository's primary set by
  hand goes back to the rule (this one is worth writing down somewhere
  outside the database), and a user's chosen home site with it;
- **the merge bases**, so ForgeSync forgets what it last agreed and
  adopts what it finds on each node — which is safe, but "deleted
  everywhere" can become "here on one node, so copy it back";
- **hand-offs in flight**, the conflict history and the acknowledgements;
- **the archived copies' deadlines**, so deleted repositories' archives
  stay until someone removes them;
- **ForgeSync's accounts and the audit log.**

## The database

It is the single point of failure: the controllers fail over, PostgreSQL
does not. ForgeSync behaves well when it's gone — a controller that can't
renew its lease stops acting *before* the lease expires, so nothing acts
on stale information, the pages stay up and say so, and `/metrics`
reports `forgesync_database_up 0` — but nothing is synced while it's
down.

ForgeSync doesn't manage this for you. Use whatever your operations
already do:

- a managed PostgreSQL with failover (simplest, and someone else's pager);
- streaming replication with a promotion tool (Patroni, repmgr), with the
  controllers pointed at whatever fronts it;
- or one server and a nightly dump, if an outage until someone restores
  it is acceptable — replication stops, nothing breaks, nobody loses
  work on the nodes.

Whichever it is, the controllers only need one URL that always reaches
the current primary. They reconnect by themselves; there is nothing to
restart after a database failover.

## Upgrades

Migrations are applied at startup under an advisory lock, so two
controllers starting at once is safe. Roll one at a time: stop it (which
gives the lease up, so the other takes over in about one renewal), put
the new binary in place, start it, watch `forgesync_leader` and the log,
then do the other.

Downgrades aren't supported: a migration that has run has run. Keep the
dump from before the upgrade.
