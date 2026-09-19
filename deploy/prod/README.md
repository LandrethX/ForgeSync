# Running ForgeSync on Debian

ForgeSync runs as a plain systemd service on Debian — no container needed. This is the
path that was actually walked on a Debian 13 LXC while writing it: every command below was
run, and what it printed is what's quoted.

Two controllers, one PostgreSQL database, and the Forgejo nodes they look after. One
controller does the work; the other serves the same pages and takes over when it stops.

| | |
|---|---|
| `forgesync.yaml` | an annotated config; copy it and change what's marked |
| `forgesyncd.service` | the systemd unit, with the usual hardening |
| `backup.sh` | takes (and can verify) a dump of the database |

---

## 1. The machine

A Debian 13 LXC with 1 vCPU and 1 GB of memory is enough for a few hundred repositories:
the controller uses about 100 MB while leading, 7 MB while standing by. It needs outbound
HTTPS to the Forgejo nodes, and the nodes need to reach it back for webhooks.

```sh
apt-get update
apt-get install -y ca-certificates curl git
```

Git is needed at runtime: ForgeSync runs the `git` CLI (2.32 or newer) for the replication
itself.

## 2. PostgreSQL

Either a database somewhere else, or on the same machine:

```sh
apt-get install -y postgresql
PW=$(openssl rand -hex 24)        # no spaces: this goes in a URL
sudo -u postgres psql -c "CREATE ROLE forgesync LOGIN PASSWORD '$PW'"
sudo -u postgres psql -c "CREATE DATABASE forgesync OWNER forgesync"
```

Keep `$PW` for the next step. A password with spaces or `@ : / ?` in it has to be
percent-encoded in the URL, so a hex string saves an argument with yourself later.

If you want `backup.sh --verify` to check its own dumps (it restores one into a scratch
database), the role needs to be allowed to make one:

```sh
sudo -u postgres psql -c "ALTER ROLE forgesync CREATEDB"
```

ForgeSync itself never creates a database, so leave this out if you'd rather verify dumps
with a role that already can.

Check which port the cluster took — Debian gives the next free one, and something else may
already hold 5432:

```sh
pg_lsclusters
# Ver Cluster Port Status Owner    Data directory
# 17  main    5433 online postgres /var/lib/postgresql/17/main
```

ForgeSync creates its own tables on first start and migrates them on every upgrade, under
an advisory lock, so two controllers starting at once is safe.

## 3. The binaries

Build them on a machine with Go 1.27 and Node 22 (the admin UI is embedded in the
controller binary), then copy the two files over:

```sh
make build                       # bin/forgesyncd and bin/forgesync
install -m 0755 bin/forgesyncd bin/forgesync /usr/local/bin/
```

`/usr/local/bin/forgesyncd -version` should print the version and the commit.

## 4. The user, the directories and the secrets

```sh
useradd --system --home /var/lib/forgesync --shell /usr/sbin/nologin forgesync
install -d -o forgesync -g forgesync -m 0750 /var/lib/forgesync /etc/forgesync
install -d -o root -g forgesync -m 0750 /etc/forgesync/secrets
```

Every secret is a file that ForgeSync reads at startup, never a value in the config or the
environment, so none of them reaches a process list, a log or a core dump:

```sh
printf 'postgres://forgesync:%s@127.0.0.1:5433/forgesync?sslmode=disable' "$PW" \
  > /etc/forgesync/secrets/database.url
openssl rand -hex 32 > /etc/forgesync/secrets/admin.token
# one API token per Forgejo node, from a site-admin account called forgesync there:
printf '%s' "$SE_TOKEN" > /etc/forgesync/secrets/se.token
chown root:forgesync /etc/forgesync/secrets/*
chmod 0640 /etc/forgesync/secrets/*
```

## 5. The config

```sh
install -m 0640 -o root -g forgesync forgesync.yaml /etc/forgesync/forgesync.yaml
$EDITOR /etc/forgesync/forgesync.yaml
```

The file beside this README is annotated; the parts that matter are the controller's name,
URL and priority, the database, the admin token, the nodes with their tokens, and which
features to replicate. On the second controller, change `controller.name`,
`controller.url`, `controller.priority` (make it 2) and `webhooks.url`; everything else,
including the database URL, stays the same. Sharing the database is what pairs them.

## 6. The service

```sh
install -m 0644 forgesyncd.service /etc/systemd/system/forgesyncd.service
systemctl daemon-reload
systemctl enable --now forgesyncd
systemctl status forgesyncd
curl -s localhost:8090/healthz     # {"status":"ok"}
```

The unit runs as the `forgesync` user with `ProtectSystem=strict`, no capabilities and a
system-call filter; it can write to `/var/lib/forgesync` and nothing else.
`systemctl reload` sends SIGHUP, which re-reads the TLS certificate without dropping
connections — and does nothing harmful when there is no certificate to re-read.

## 7. Signing in

ForgeSync has accounts of its own, in the shared database, so they work on either
controller. The first is made with the admin token, there being nobody to make it
otherwise:

```sh
curl -X POST -H "Authorization: Bearer $(cat /etc/forgesync/secrets/admin.token)" \
     -H 'Content-Type: application/json' \
     -d '{"username":"you","password":"a long passphrase","role":"administrator"}' \
     http://localhost:8090/api/v1/accounts
```

After that it's username and password in the UI. SceneID signs people in to the *nodes*; it
has nothing to do with the controllers.

The command-line client uses the same admin token:

```sh
export FORGESYNC_SERVER=http://localhost:8090
export FORGESYNC_TOKEN_FILE=/etc/forgesync/secrets/admin.token
forgesync node list
forgesync repo list --state differs
forgesync conflict list
```

## 8. TLS

Either something in front terminates it — leave the `tls_*` settings out and let the proxy
talk HTTP to `listen` — or ForgeSync does it itself:

```yaml
http:
  listen: 0.0.0.0:8090        # plain HTTP, for a proxy on this host
  tls_listen: 0.0.0.0:8443    # and HTTPS for browsers, at the same time
  tls_cert_file: /etc/forgesync/tls/fullchain.pem
  tls_key_file: /etc/forgesync/tls/privkey.pem
```

Without `tls_listen`, the certificate takes `listen` over. The pair is checked at startup
(both files, and they must make a keypair) and re-read on `systemctl reload`, so a renewal
needs no restart; a pair that won't load leaves the one in use rather than taking the
controller off the air. Point your ACME client's deploy hook at `systemctl reload
forgesyncd`.

## 9. The Forgejo nodes

Each node needs a site-admin account for ForgeSync — `forgesync` — with an API token, and
the controllers' hosts in `[webhook] ALLOWED_HOST_LIST` so the webhooks can reach whichever
is leading. ForgeSync installs one system webhook per node itself and keeps it right.

## 10. Watching it

`/metrics`, Prometheus text format, needs the Viewer role, so the scraper sends the admin
token as a bearer token. What to alert on, in order:

| Series | Why |
|---|---|
| `forgesync_leader` | exactly one controller should be acting. Zero means nobody is syncing, whatever the pages say |
| `forgesync_database_up` | a controller that has lost the database stops acting before its lease runs out |
| `forgesync_node_healthy` | a node that isn't answering isn't being replicated to |
| `forgesync_scan_last_success_timestamp_seconds` | a scan that stops is a sync that stops, and nothing else shows it |
| `forgesync_replicas{state}` | anything that isn't `synced` is waiting for a person or a retry |
| `forgesync_conflicts_open` | differences ForgeSync won't decide on its own |

A round logs its own duration; `log.level: debug` adds a line per part, which is how you
find out what to change when a round outgrows `inventory.interval`.

`prometheus/` has the scrape job and ten alert rules to start from:

```sh
cp prometheus/forgesync.rules.yml /etc/prometheus/rules/
cp /etc/forgesync/secrets/admin.token /etc/prometheus/forgesync.token   # the scraper signs in with it
# then merge prometheus/scrape.yml into your prometheus.yml
```

The rules aggregate across both controllers rather than looking at one, because a standby
correctly reports that it isn't leading -- an alert per instance would page you for that.
The thresholds follow the settings they're about, named in a comment on each rule; the scan
one assumes `inventory.interval: 30m` and wants changing with it.

They were checked with `promtool` and then against this installation: all ten load and
evaluate, and stopping a node made `ForgeSyncNodeUnhealthy` fire with the node's name in it
and resolve when it came back.

## 11. Backups

The database is the only thing that can't be rebuilt: what each repository's primary is,
what ForgeSync last wrote to every replica, the merge bases behind every "one new value
wins" decision, the conflicts people are working through, the archived copies of deleted
repositories, ForgeSync's accounts and the audit log.

```sh
./backup.sh /var/backups/forgesync            # nightly, from a timer
./backup.sh /var/backups/forgesync --verify   # weekly: restores it into a scratch database
```

Not backed up, on purpose: the git cache under `replication.work_dir` (it is a cache —
delete it and the next run refetches, and don't share it between controllers), the Forgejo
nodes (whoever runs them backs those up), and the secrets (they belong wherever your
secrets live).

### Restoring

```sh
systemctl stop forgesyncd            # on BOTH controllers
psql "$URL/postgres" -c 'DROP DATABASE forgesync WITH (FORCE)'
psql "$URL/postgres" -c 'CREATE DATABASE forgesync OWNER forgesync'
pg_restore --dbname="$FORGESYNC_DATABASE_URL" --no-owner forgesync-....dump
systemctl start forgesyncd           # one controller first, then the other
```

Then check that the repository count is what it was, `forgesync_replicas` is all `synced`,
and no conflicts appeared that weren't there before. Tried on the test environment: three
repositories, twelve replicas and one open conflict before; the same three, twelve and one
after, accounts still signing in, no errors.

### If the database is lost with no backup

Nothing on the nodes is damaged, and ForgeSync rebuilds most of itself from them: it
rediscovers every repository, assigns primaries by the rules, and replication carries on.
Tried too — from an empty database the test environment came back with all replicas in sync
and nothing overwritten.

What is gone is everything nobody can infer from the nodes: **every choice a person made**
(a primary set by hand goes back to the rule — worth writing down somewhere outside the
database — and a user's chosen home site with it), the merge bases (so ForgeSync adopts
what it finds, and "deleted everywhere" can become "here on one node, so copy it back"),
hand-offs in flight, the conflict history, the archived copies' deadlines, ForgeSync's
accounts and the audit log.

## 12. The database is the single point of failure

The controllers fail over; PostgreSQL doesn't. ForgeSync behaves well when it's gone — a
controller that can't renew its lease stops acting before the lease expires, so nothing acts
on stale information — but nothing is synced while it's down. ForgeSync doesn't manage this;
use what your operations already do:

- a managed PostgreSQL with failover (simplest, and someone else's pager);
- streaming replication with a promotion tool (Patroni, repmgr), the controllers pointed at
  whatever fronts it;
- or one server and a nightly dump, if an outage until someone restores it is acceptable —
  replication stops, nothing breaks, nobody loses work on the nodes.

## 13. Upgrades

Migrations run at startup under an advisory lock. Roll one controller at a time: stop it
(which gives the lease up, so the other takes over in about one renewal), put the new binary
in place, start it, watch `forgesync_leader` and the log, then do the other. Downgrades
aren't supported — a migration that has run has run — so keep the dump from before.
