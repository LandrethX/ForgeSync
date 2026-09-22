# Running ForgeSync on Debian

ForgeSync runs as a plain systemd service on Debian, with no container needed. This is the
path that was actually walked on a Debian 13 LXC while writing it: every command below was
run, and what it printed is what's quoted.

One or more controllers, one PostgreSQL database between them, and the Forgejo nodes they
look after. One controller does the work; the rest serve the same pages and take over when
it stops. Section 12 says how many machines to run and why the answer is one or three
rather than two.

| File | What it is |
|---|---|
| `install.sh` | does sections 1 to 7 of this file in one command |
| `standby.sh` | a second copy of the database on two machines, promoted by a person |
| `cluster.sh` | the three-machine database, which promotes itself |
| `forgesync.yaml` | an annotated config; copy it and change what's marked |
| `forgesyncd.service` | the systemd unit, with the usual hardening |
| `backup.sh` | takes (and can verify) a dump of the database |

## The short way

Take the installer from the release you are installing, not from `main`, so the script and
the build it installs are the pair that were tested together. Read it before running it as
root; it is written to be read:

```sh
VERSION=v0.11.0
curl -fsSLO "https://raw.githubusercontent.com/LandrethX/ForgeSync/$VERSION/deploy/prod/install.sh"
less install.sh
bash install.sh --first --binary --ref "$VERSION"
```

`--binary` installs the published build and compiles nothing, so no Go, Node or compiler is
fetched or left behind. Leave it off to clone and build here instead.

Piping it straight into `bash` works too, and then there is nothing to type into, so answer
in advance: it otherwise asks whether this is the first ForgeSync machine, and if it is not,
where the first one is. `--first` and `--join <address>` are those two answers.

### Adding a second or third machine

Controllers share one database, and three of their secrets have to be identical or the
installation only half works: the CLI would authenticate against one machine and not the
others, each leader would rewrite every node's webhook on taking over, and the node tokens
sealed in the database would not open. So the first machine hands them over as one file:

```sh
bash install.sh --join-bundle              # on the first machine
scp /root/forgesync-join.txt root@<new machine>:/root/
bash install.sh --bundle /root/forgesync-join.txt   # on the new one
```

The bundle carries the database URL, the admin token, the webhook secret, the node key, and
where the first machine is. The new machine points the database URL at the first machine
rather than its own loopback, and takes the next free `controller.priority` by asking the
first one which are already taken.

**That file is the keys to the whole installation.** It is written 0600, and the script
shuts it to 0600 again on arrival because `scp` does not preserve the mode. Move it over ssh
and delete it from both machines afterwards; it does not expire. If you would rather not
have such a file exist at all, copy the four files out of `/etc/forgesync/secrets` yourself
and run `install.sh --join <address>`, which waits for them to appear.

PostgreSQL on the first machine has to accept the connection: `listen_addresses` in
`postgresql.conf`, a `host` line for the new machine in `pg_hba.conf`, then reload it. The
second machine gives you a controller that survives losing the first; the database is still
one server until there are three (section 12).

**Running it again is how you upgrade**, either way in, and nothing already there is
overwritten: secrets are made only when missing, the config only when missing, and the
database role and database only when they do not exist.

Left off, `--binary` becomes a build on this machine, which takes about 48 seconds and 400
MB of build space and gives the space back at the end unless you pass `--keep-build`. That
is the path that wants 2 GB of memory; a `--binary` install does not, and section 1 has the
measurements for both.

The rest of this file is what it does, in order, and is the reference when something needs
doing by hand or looking at afterwards.

---

## After install.sh finishes

The installer leaves a controller running and answering on plain HTTP, with no accounts and
no nodes. It has done sections 1 to 6. What is left is the part only you can decide: who
signs in, what address people reach it at, and what stands in front of it.

Ten steps, in this order, because each one depends on the one before.

**1. Check it is actually up.** `/readyz` means the database is reachable and takes writes,
which is more than `/healthz` claims:

```sh
curl -fsS http://localhost:8090/readyz          # {"status":"ready"}
```

**2. Make the first administrator.** There is nobody to make it through the UI, so the admin
token does it. **The password must be at least 12 characters**, and a shorter one comes back
`400`. Section 7 has a form that keeps it out of your shell history.

**3. Decide the address people will use**, and put it in `controller.url`. The installer
wrote whatever `--url` said, which on a first install is usually an IP and a port. This is
the address the other controller's UI sends people to and the one the dashboard shows. It is
**not** the address this process listens on: see the table in section 8.

**4. Put TLS in front of it**, or on it. Section 8 has both, including a worked reverse
proxy example.

**5. Set `secure_cookies: true`.** The installer writes `false`, deliberately, because on
the day it runs there is no TLS and a Secure cookie would simply never arrive. Once there is
TLS, this stops the session cookie travelling in the clear. It is the one setting whose
default is safe and whose installed value is not.

**6. Set `trusted_proxies`** if anything is in front of it. Without this every request looks
as though it came from the proxy, which breaks the audit trail and the sign-in limiter in a
way that is explained in section 8, because the consequence is worse than it sounds.

**7. Point `webhooks.url` at the same public address** as step 3, and restart:

```sh
systemctl restart forgesyncd
curl -fsS https://forgesync.example.org/readyz
```

**8. Back up the node key somewhere other than the database dump.** Section 4 says why: a
backup without it is a database full of node tokens nobody can open.

**9. Prepare the first Forgejo node.** It needs a site-admin account for ForgeSync, a token
for it, and two `app.ini` keys that no API can reach. Section 9, and the admin UI prints the
same checklist when you add one.

**10. Add the node in the UI**, and watch a round. Section 9 again.

### Post-install checklist

```text
[ ] /readyz returns ready
[ ] First administrator created (12 characters or more)
[ ] controller.url is the production address
[ ] webhooks.url is the production address
[ ] TLS or a reverse proxy in front
[ ] secure_cookies: true
[ ] trusted_proxies lists the proxies, and only the proxies
[ ] node-key backed up away from the database dump
[ ] Signed in to the UI with the account from step 2
[ ] Forgejo can reach this machine, and this machine can reach Forgejo
[ ] First Forgejo node prepared (service account, token, app.ini)
[ ] First Forgejo node added in the UI
[ ] A scan round finished and the node shows healthy
[ ] Database backup scheduled (section 11)
```

The installer prints this too, so it is in front of you at the moment it matters.

## 1. The machine

An unprivileged Debian 13 LXC. In Proxmox, `Create CT` with:

| Setting | Value | Why |
|---|---|---|
| Template | `debian-13-standard` | What the install script checks for and refuses without |
| Unprivileged | **yes** | Nothing here wants root on the host. Leave the default alone |
| Nesting, FUSE, keyctl | off | Not needed. PostgreSQL, git and Go all run without them |
| Cores | 2 | 1 runs it. A source build is the only thing that wants the second |
| Memory | **512 MB** with `--binary`, **2048 MB** to build from source. Swap 512 either way | Two different machines; see below |
| Disk | **16 GB** to start | A fresh `--binary` install is 1.35 GB of it. The git cache is what grows; a source build also wants 3 GB while it works |
| Network | a fixed address | The Forgejo nodes reach it for webhooks and the other machines name it in `database.url`, so it must not move. A static address or a DHCP reservation |
| Start at boot | yes | |

Then, inside it:

```sh
apt-get update
apt-get install -y ca-certificates curl git
```

Git is needed at runtime, not just to build: ForgeSync runs the `git` CLI (2.32 or newer)
for the replication itself.

### What it actually uses

Measured, on the five-node test installation and on the install itself.

### Memory: how 512 MB and 2 GB are both right

They are answers to different questions, and the table above gives both because the old
single number was only ever justified by the build.

**If you install with `--binary`, nothing is compiled and 512 MB is generous.** Measured on a
real 512 MB Proxmox LXC, installed and running, idle with no nodes added yet:

| What | Measured |
|---|---|
| Memory, as Proxmox's own gauge reports it | **52 MiB of 512, about 10%** |
| Memory used, as `top` inside it reports | 57 MB, with 455 MB available |
| Swap used, of 512 MB | none |
| Disk | **1.35 GiB**, on a 31 GB volume |
| `forgesyncd` itself | 16 MB resident |
| PostgreSQL, all its processes together | about 30 MB resident, most of it shared between them |

Proxmox and `top` agree because both leave out page cache; cgroup accounting counts the
cache too and so reports around 157 MB for the same machine. The cache is reclaimable, so
the number worth watching is the 455 MB still available.

**If you build from source, 2 GB.** The build is the busiest this machine ever gets: the
admin UI build alone peaks at 184 MB and the Go compiler wants its own, on top of everything
above, and it needs about 3 GB of disk while it works. This is the only reason the number
was ever 2 GB.

**What makes it grow is repositories, not the install.** The controller's own memory is its
working set during a round, and a round compares every repository against every replica:

| What | Measured |
|---|---|
| Idle, no nodes | 16 MB |
| Leading, 203 repositories on five nodes | about 100 MB |
| Leading, at the peak of a round over 1000 repositories | **300 MB** |
| Standing by, whatever the size | 6 to 7 MB |

So 512 MB is right for a first machine and a modest installation, with room to spare. Around
a thousand repositories it becomes the controller's 300 MB plus PostgreSQL's share, which is
close enough to 512 that a gigabyte is the comfortable answer. A standby that never leads
stays at the bottom of that range whatever you give it, so on three machines only the one
holding the lease needs the headroom.

### What else it uses

| What | Measured |
|---|---|
| PostgreSQL with ForgeSync's state | 12 MB of data for 203 repositories, 812 replicas; 16 MB at 1000 |
| The two binaries | 23 MB, and with `--binary` that is all |
| The Go toolchain (a source build only, kept for the next upgrade) | 282 MB |
| Node (a source build only, same reason) | about 120 MB |
| A source build: module cache, build cache, node_modules | about 400 MB, given back afterwards unless `--keep-build` |
| A clean install from a release (`--binary`) | **25 seconds**, no toolchain |
| A clean install that builds from source | 48 seconds |

**Disk is the thing to watch, and it is the git cache that grows.** Under
`replication.work_dir` a controller keeps a bare mirror of every repository it has replicated,
so plan for roughly the total size of your repositories, plus their history. It was 51 MB for
203 small test repositories, which tells you the shape and not the size: work yours out from
what your Forgejo nodes are using. A standby that has never led has almost nothing there, and
fills it the first time it takes over.

On a three-machine installation (section 12) each machine also carries PostgreSQL and etcd.
etcd is tiny. The database is the size above. It is still the git cache that decides.

If the disk does fill, nothing is lost: replication stops with errors, the pages stay up, and
it resumes when there is room. The cache can be deleted and will be rebuilt, which is slow
but safe.

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

Check which port the cluster took. Debian gives the next free one, and something else may
already hold 5432:

```sh
pg_lsclusters
# Ver Cluster Port Status Owner    Data directory
# 17  main    5433 online postgres /var/lib/postgresql/17/main
```

ForgeSync creates its own tables on first start and migrates them on every upgrade, under
an advisory lock, so several controllers starting at once is safe.

## 3. The binaries

Three ways, in the order most people will want them.

**From a release, which is what `install.sh --binary` does.** Nothing is compiled, so this
machine never sees Go, Node or a compiler:

```sh
V=v0.11.0                                   # the current release
curl -fsSLO "https://github.com/LandrethX/ForgeSync/releases/download/$V/SHA256SUMS"
curl -fsSLO "https://github.com/LandrethX/ForgeSync/releases/download/$V/forgesync-$V-linux-amd64.tar.gz"
sha256sum -c --ignore-missing SHA256SUMS
tar xzf "forgesync-$V-linux-amd64.tar.gz"
install -m 0755 forgesync-*/forgesyncd forgesync-*/forgesync /usr/local/bin/
```

Each tarball also carries this directory, so the systemd unit, the annotated config,
`backup.sh` and `cluster.sh` come with it.

**Built here**, which is what you want while working on ForgeSync or running an untagged
commit. Needs Go 1.27 and Node 22, about 3 GB of room while it works, and it keeps roughly
400 MB of toolchain afterwards:

```sh
make build                       # bin/forgesyncd and bin/forgesync
install -m 0755 bin/forgesyncd bin/forgesync /usr/local/bin/
```

**Built elsewhere and copied**, if this machine should stay clean but there is no release:
`make release` on a build host produces the same tarballs, for `linux/amd64` and
`linux/arm64`, with a `SHA256SUMS` beside them.

`/usr/local/bin/forgesyncd -version` should print the version and the commit, whichever way
you got here.

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
# the key that seals node tokens in the database (see below):
openssl rand -hex 32 > /etc/forgesync/secrets/node-key
# one API token per Forgejo node, from a site-admin account called forgesync there:
printf '%s' "$SE_TOKEN" > /etc/forgesync/secrets/se.token
chown root:forgesync /etc/forgesync/secrets/*
chmod 0640 /etc/forgesync/secrets/*
```

### The node key

Nodes live in ForgeSync's database, so adding one is a single write every controller sees
rather than a file edited on each machine and a restart of each. The token that comes with
a node is the one secret that then has to be written down, and it is sealed with this key
before it goes in: the database holds ciphertext, and the key is only ever on the
controllers' disks.

Three things follow, and all three matter:

- **Every controller needs the same key file.** Copy it when you set the second and third
  machines up (section 12). A controller with the wrong key cannot open the tokens and says
  so at startup rather than running with fewer nodes than the installation has.
- **Back it up somewhere other than the database dump**, or the two are lost together and
  the backup is worth less than it looks. A password manager or a sealed envelope is fine;
  it is 32 bytes.
- **Losing it is recoverable**, unlike losing the database: enter the node tokens again and
  ForgeSync seals them under a new key. That is a bad afternoon, not a disaster.

Leaving `node_key_file` out is allowed and changes nothing: nodes then come from the config
file and their tokens from the files above, exactly as they always did. Adding a node from
the UI is what needs the key.

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
connections, and does nothing harmful when there is no certificate to re-read.

## 7. Signing in

ForgeSync has accounts of its own, in the shared database, so they work on either
controller. The first is made with the admin token, there being nobody to make it
otherwise.

**The password must be at least 12 characters.** That is the only rule: length is what makes
a password hard to guess, and rules about punctuation mostly make them hard to remember. A
shorter one comes back `400` with
`{"message":"the password must be at least 12 characters"}`, which is worth knowing before
you wonder what else you got wrong.

This form asks for the password rather than taking it from the command line, so it reaches
neither your shell history nor the process list:

```sh
read -rsp 'New ForgeSync password (12 characters or more): ' FS_PW; echo
curl -fsS -X POST http://localhost:8090/api/v1/accounts \
  -H "Authorization: Bearer $(cat /etc/forgesync/secrets/admin.token)" \
  -H 'Content-Type: application/json' \
  --data-binary @- <<JSON
{"username":"you","password":"$FS_PW","role":"administrator"}
JSON
unset FS_PW
```

A password with a `"` or a `\` in it would need escaping for JSON; a passphrase of ordinary
words avoids the question. The one-line form is fine where history does not matter:

```sh
curl -X POST -H "Authorization: Bearer $(cat /etc/forgesync/secrets/admin.token)" \
     -H 'Content-Type: application/json' \
     -d '{"username":"you","password":"a long passphrase","role":"administrator"}' \
     http://localhost:8090/api/v1/accounts
```

After that it's username and password in the UI. SceneID signs people in to the *nodes*; it
has nothing to do with the controllers.

### Checking the account from the command line

The UI is the point, but it is worth proving the account works before opening a browser.
Signing in is a write, so it needs the CSRF header that cookie-authenticated writes need;
without it you get `403 missing X-ForgeSync-CSRF header`, which looks like a rejected
password and is not one. The value is not checked, only its presence:

```sh
curl -fsS -X POST http://localhost:8090/api/v1/session \
  -H 'Content-Type: application/json' -H 'X-ForgeSync-CSRF: 1' \
  -c /tmp/fs-cookie -d '{"username":"you","password":"a long passphrase"}'
curl -fsS -b /tmp/fs-cookie http://localhost:8090/api/v1/overview
rm -f /tmp/fs-cookie
```

`200` and an overview naming this controller means the account is good. A wrong password is
`401`, which is the answer that means what it says.

The command-line client uses the same admin token:

```sh
export FORGESYNC_SERVER=http://localhost:8090
export FORGESYNC_TOKEN_FILE=/etc/forgesync/secrets/admin.token
forgesync node list
forgesync repo list --state differs
forgesync conflict list
```

## 8. TLS, and what is in front of it

### Five settings people confuse

Four of these name an address and they mean different things. Getting them mixed up is the
commonest way an installation half works.

| Setting | What it means |
|---|---|
| `controller.url` | The **public** address of *this* controller. Advertised, never listened on: it is what the other controller's UI links to and what the dashboard shows |
| `http.listen` | The **local** address and port `forgesyncd` binds. Behind a proxy this stays plain HTTP on `0.0.0.0:8090` and does **not** become 443 |
| `webhooks.url` | The **public** callback the Forgejo nodes post to. Normally `controller.url` plus `/api/v1/hooks/forgejo` |
| `http.trusted_proxies` | The proxies whose `X-Forwarded-For` is believed. Only the proxies, never a range users sit in |
| `http.secure_cookies` | Whether the session cookie is marked Secure, so browsers send it over HTTPS only. Defaults to true; the installer writes false because on install day there is no TLS |

The trap is `http.listen`. Terminating TLS at a proxy does not change where the controller
listens: the proxy takes 443 and speaks plain HTTP to 8090 behind it. Only `controller.url`
and `webhooks.url` become `https://`.

### Behind a reverse proxy

This is what most installations look like. The proxy holds the certificate, ForgeSync does
not:

```text
        browser / Forgejo node
                 │  HTTPS :443
                 ▼
         reverse proxy  (10.46.225.10)
                 │  HTTP :8090
                 ▼
            forgesyncd
```

`/etc/forgesync/forgesync.yaml`, with a proxy on `10.46.225.10`:

```yaml
controller:
  name: fsync-mlm
  url: https://fsync-mlm.example.net          # public, advertised
  priority: 1

http:
  listen: 0.0.0.0:8090                        # local, still plain HTTP
  admin_token_file: /etc/forgesync/secrets/admin.token
  secure_cookies: true                        # the proxy terminates TLS
  trusted_proxies:
    - 10.46.225.10                            # the proxy, and nothing else

webhooks:
  url: https://fsync-mlm.example.net/api/v1/hooks/forgejo
  secret_file: /etc/forgesync/secrets/webhook.secret
```

Then `systemctl restart forgesyncd`, because none of those are re-read on a reload; `SIGHUP`
re-reads the certificate only.

What the proxy needs to do: forward to `http://<controller>:8090`, pass `X-Forwarded-For`,
and allow WebSocket-style streaming responses for `/api/v1/events`, which is Server-Sent
Events and must not be buffered. In **Nginx Proxy Manager** that is a Proxy Host with the
scheme `http`, the forward port `8090`, *Websockets Support* on, and in Advanced:

```nginx
proxy_buffering off;          # /api/v1/events is a live stream
proxy_read_timeout 3600s;     # it stays open
```

NPM sets `X-Forwarded-For` itself. Whatever proxy you use, the address in
`trusted_proxies` is the one ForgeSync sees the connection **come from**, which is the
proxy's address on the network between them, not the address people type.

### Why `trusted_proxies` is not optional behind a proxy

Leave it out and every request appears to come from the proxy, which costs two things that
are easy to miss until they bite.

The **audit history** records the proxy's address against every sign-in, so the one question
it exists to answer, who signed in from where, has the same answer for everybody.

The **sign-in limiter** counts failures per client address, ten in five minutes. With one
address for everyone, ten wrong passwords from anyone lock out the whole installation for
five minutes, including you, and the obvious remedy of trying again makes it worse.

The rule is to name the proxies and nothing else. What the setting decides is *whose claim
about somebody else's address is believed*, so listing a range that users also sit in lets
any of them write whatever address they like into the history and dodge the limiter with it.
A bare address means itself (`10.46.225.10` becomes `10.46.225.10/32`); CIDRs are allowed for
a range that holds only proxies:

```yaml
http:
  trusted_proxies:
    - 10.46.225.10            # the proxy
    - 127.0.0.1               # or a proxy on this host
```

`X-Forwarded-For` is read from the right, because each proxy appends the address it saw: the
right-most address that is not one of yours is the client. Anything further left was supplied
by whoever connected and proves nothing. A header ForgeSync cannot parse makes it fall back
to the connection's own address rather than trust part of it.

### Checking it end to end

```sh
curl -fsS https://fsync-mlm.example.net/readyz                    # through the proxy
curl -fsS http://localhost:8090/readyz                            # and directly
```

Sign in through the proxy, then look at the history: your own address should appear against
the sign-in, not the proxy's. If it shows the proxy, `trusted_proxies` is wrong or missing,
and the paragraph below says why that matters more than it looks.

### ForgeSync holding the certificate itself

Either something in front terminates it, in which case you leave the `tls_*` settings out
and let the proxy talk HTTP to `listen`, or ForgeSync does it itself:

```yaml
http:
  listen: 0.0.0.0:8090        # plain HTTP, for a proxy on this host
  tls_listen: 0.0.0.0:8443    # and HTTPS for browsers, at the same time
  tls_cert_file: /etc/forgesync/tls/fullchain.pem
  tls_key_file: /etc/forgesync/tls/privkey.pem
```

`Strict-Transport-Security` is sent on requests that arrive over TLS, so behind a proxy the
proxy has to send it: what reaches ForgeSync is plain HTTP.

Without `tls_listen`, the certificate takes `listen` over. The pair is checked at startup
(both files, and they must make a keypair) and re-read on `systemctl reload`, so a renewal
needs no restart; a pair that won't load leaves the one in use rather than taking the
controller off the air. Point your ACME client's deploy hook at `systemctl reload
forgesyncd`.

## 9. The Forgejo nodes

Nodes live in ForgeSync's own database, so one is added from the admin UI (**Nodes**, then
**Add a node**) and every controller has it. There is no list to edit on each machine and no
service to restart by hand.

### What has to reach what

Worth settling before anything else, because a node that cannot be reached looks exactly
like a node that is broken. Everything ForgeSync does to a node goes over **one port**: the
one in that node's URL, normally 443. There is no SSH anywhere in the controller, and git
replication uses the same HTTPS endpoint as the API, with the token in a header.

| From | To | Port | Why |
|---|---|---|---|
| Administrator | proxy, or the controller | 443, or 8090 without a proxy | The UI and the API |
| **Forgejo node** | proxy, or the controller | 443, or 8090 | Webhooks. This is the one people forget, and without it everything still works, just minutes late instead of seconds |
| Controller | **Forgejo node** | 443 | The REST API, git replication and LFS, all on the node's own URL |
| Controller | PostgreSQL | 5432 | Only when the database is not on this machine |
| Controller | the other controllers | 5432 | They share one database, so really this is the row above |
| Controller | 1.1.1.1, 9.9.9.9, 8.8.8.8 | 53/udp | Only to tell "every node is down" from "our network is down". `health.uplink_check: []` turns it off |
| Controller | github.com | 443 | Installing and upgrading only, never at runtime |

On three machines add etcd between the controllers, 2379 and 2380, and PostgreSQL's 5432
between them for the replication (section 12).

Two consequences worth stating plainly. Webhooks are what make a push appear on the other
nodes in seconds rather than at the next scan, so a one-way firewall that lets ForgeSync
reach the nodes but not the reverse leaves an installation that works and feels slow, with
nothing obviously wrong. And the node must have this controller's hostname in its
`[webhook] ALLOWED_HOST_LIST`, which is a Forgejo setting no API can reach: see below.

### What the node needs first

ForgeSync never changes a node's own configuration, and two of these are `app.ini` keys that
no API can reach and that need Forgejo restarted. Do them on the node:

```sh
forgejo admin user create --admin --username forgesync --email forgesync@example.org
forgejo admin user generate-access-token --username forgesync \
  --token-name forgesync --scopes all --raw
forgejo admin auth list            # note the SceneID login source id
```

```ini
[webhook]
ALLOWED_HOST_LIST = your-forgesync-host   ; so the node can report changes back
[server]
LFS_START_SERVER  = true                  ; or large files cannot be carried
```

Then restart Forgejo. ForgeSync installs one system webhook per node itself and keeps it
right; you only have to let the node reach the controller.

### Adding it

Give the UI the name, the address and that token, and press **Check**. ForgeSync asks the
node what it is and says what it found: whether it answers, whether the token works, who the
token belongs to, and whether that account is a site admin. Nothing is stored until every
blocking check passes, so a node that cannot be used fails while you are looking at it rather
than on the next replication round.

The token is sealed with the node key (section 4) before it is written, and is never shown
again. **Check** writes nothing and can be run as often as you like while working through
what the node still needs.

A controller picks a new node up within `inventory.node_check` (15s by default): it stops,
and whatever runs it starts it again a second later. That costs nothing here, because every
loop is idempotent and another controller keeps serving meanwhile. Measured on the test
environment: a node retired was gone in 12 seconds, and one added was in use 6 seconds later.

### Taking one out

**Retire** on the node's page. Nothing watches, scans or replicates to it afterwards. The row
stays, because its health history is part of the audit trail and nothing may edit that, and
because a node taken out and put back should not come back a stranger. Adding it again brings
it back with what is known about it.

A node that is still listed in a controller's config file is **not** taken back in by that
file: retiring is an explicit act and sticks, and the controller says in its log that the
file names a node that has been retired.

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

The rules aggregate across the controllers rather than looking at one, because a standby
correctly reports that it isn't leading -- an alert per instance would page you for that.
The thresholds follow the settings they're about, named in a comment on each rule; the scan
one assumes `inventory.interval: 30m` and wants changing with it.

They were checked with `promtool` and then against this installation: all ten load and
evaluate, and stopping a node made `ForgeSyncNodeUnhealthy` fire with the node's name in it
and resolve when it came back.

**None of these watch the database itself**, because ForgeSync does not: `forgesync_database_up`
says a controller cannot reach it, not why. On a three-machine installation (section 12)
`cluster.sh status --quiet` is the check for that half. It prints nothing and exits 0 while
the cluster is in a state you would be happy to be left alone with, and otherwise prints
what is wrong and exits non-zero, so cron or any monitoring that reads an exit code can call
it:

```sh
*/5 * * * * root /usr/local/src/forgesync/deploy/prod/cluster.sh status --quiet
```

What counts as wrong: no leader or more than one, a member that is not running, a replica
more than `FORGESYNC_MAX_LAG_MB` (256 by default) behind, or an etcd member count that
cannot survive losing a machine. Without `--quiet` it prints the same checks in full, with
`patronictl list` above them.

## 11. Backups

The database is the only thing that can't be rebuilt: what each repository's primary is,
what ForgeSync last wrote to every replica, the merge bases behind every "one new value
wins" decision, the conflicts people are working through, the archived copies of deleted
repositories, ForgeSync's accounts and the audit log.

```sh
./backup.sh /var/backups/forgesync            # nightly, from a timer
./backup.sh /var/backups/forgesync --verify   # weekly: restores it into a scratch database
```

Not backed up, on purpose: the git cache under `replication.work_dir` (it is a cache:
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
Tried too: from an empty database the test environment came back with all replicas in sync
and nothing overwritten.

What is gone is everything nobody can infer from the nodes: **every choice a person made**
(a primary set by hand goes back to the rule, which is worth writing down somewhere outside
the database, and a user's chosen home site with it), the merge bases (so ForgeSync adopts
what it finds, and "deleted everywhere" can become "here on one node, so copy it back"),
hand-offs in flight, the conflict history, the archived copies' deadlines, ForgeSync's
accounts and the audit log.

## 12. How many servers, and losing one

ForgeSync grows one machine at a time, and each step buys something different. A machine is
always the same thing: PostgreSQL, the controller, and the git cache. There is no separate
kind of server to install.

### One

PostgreSQL and the controller on it, which is sections 2 to 6 above. Nothing is redundant,
and that is a perfectly reasonable place to start: nothing is lost when it stops, because the
work lives on the Forgejo nodes. Replication simply waits.

### Two

A second machine running only the controller, pointed at the first machine's PostgreSQL
(section 5 says what to change). The two share the lease, so whichever holds it does the work
and the other stands by and serves the same pages. You gain a controller that survives losing
the other one, and the ability to upgrade or restart either without an outage.

You do **not** yet gain a database that survives anything: it is still one server, on the
first machine. Do not put a second PostgreSQL here and replicate between the two. Two servers
cannot fail over safely: a majority of two is both of them, so nothing can tell "the other is
dead" from "I cannot reach the other", and a pair that promotes on its own judgement ends up,
in a network partition, with two primaries and two divergent databases. PostgreSQL will not
merge them afterwards; you keep one and lose everything decided in the other, which includes
every manually chosen primary, every dismissed conflict and every merge base.

### Two, with a second copy of the database

A second machine gives you a controller that survives losing the first. It can also keep a
continuously updated copy of the database, which is worth having on its own: losing the first
machine then loses nothing, instead of losing everything since last night's dump.

```sh
./standby.sh prepare <address of the second>     # on the first machine
scp /root/forgesync-standby.txt root@<second>:/root/
./standby.sh create /root/forgesync-standby.txt  # on the second
./standby.sh status                              # either, any time
```

`prepare` makes a replication role and a slot, opens PostgreSQL to the two machines, and
rewrites `database.url` to name both servers. `create` copies the database across with
`pg_basebackup` and starts it as a standby. Both then point at whichever server takes writes,
so a promotion needs nothing changed anywhere.

When the first machine is gone:

```sh
./standby.sh promote     # on the second
```

It asks you to confirm, promotes the database, restarts the controller and waits for it to
be ready. Measured on two machines: **9 seconds** from the command to a controller leading
again with everything written before the failure still there.

**Promotion is deliberately yours to order, not automatic.** With two machines nothing can
tell "the other one is dead" from "I cannot reach the other one", and a pair that promotes on
its own judgement ends up, in a network partition, with two primaries and two histories that
will not merge. So this arrangement removes the data loss and leaves you the decision, which
is the trade worth making when a pause costs nothing: while the database is down the pages
stay up, the Forgejo nodes keep serving, and nothing is lost.

Two things to know afterwards. The old machine must not come back as a primary: rebuild it as
a standby of the new one, with `prepare` on the promoted machine and `create` on the old one.
And a standby is not a backup, because it faithfully reproduces a mistake: keep `backup.sh`
running as well.

### Three

Now the database can be made redundant without anybody being woken up. `cluster.sh` does it:
etcd holding the decision, Patroni making it, all from Debian's own packages.

```sh
./cluster.sh init <second> <third>          # on the first machine
scp /root/forgesync-cluster.txt root@<second>:/root/   # and to the third
./cluster.sh join /root/forgesync-cluster.txt          # on each of the others
./cluster.sh status                                    # any machine, any time
./cluster.sh status --quiet                            # the same, for monitoring (section 10)
./cluster.sh switchover                                # a planned handover
```

`init` takes a **verified dump before it touches anything** and refuses to go on if it
cannot verify it, because handing a live database to another piece of software is the one
thing here worth being frightened of. It then hands the existing PostgreSQL to Patroni: the
data is not copied or reloaded, it is the same database, adopted. `join` adds the machine to
etcd as a **learner** first, so a join that stops halfway cannot wedge the cluster, promotes
it once it has caught up, and lets Patroni clone the database from the leader.

Measured on three machines: the first machine's database was adopted with everything in it,
the primary machine was then **stopped outright**, and Patroni promoted another in about
half a minute with nobody doing anything. The two surviving controllers never went down at
all, because `database.url` names all three and a standby refuses a connection that wants to
write, so they simply reconnected to whichever had become the primary. Starting the dead
machine again brought it back as a replica five seconds later, unattended.

Three things to know. Running these again is safe and is how you finish an interrupted join.
`pg_ctlcluster` must not be used on a cluster Patroni owns, and Debian's own PostgreSQL unit
is disabled so the two cannot both start it. And while only two machines have joined, the
database is **less** resilient than one machine was, not more: a majority of two is both of
them. `status` says so plainly until the third arrives.

`standby.sh` is the two-machine arrangement and is superseded by this one: Patroni does the
replication, the promotion and the rejoining. PostgreSQL on all three, streaming replication
between them, Patroni deciding which is the primary, and etcd holding that decision with one
member per machine. Any one machine can be lost, including whichever holds the primary, and
the rest promote a new one and carry on without anybody being woken up.

Three is the number for the same reason Proxmox wants three nodes in a cluster: a majority of
three is two, so one can go. It is worth being blunt about the counts, because the intuition
is wrong:

| Machines | A majority is | Survives losing | Worth it |
|---|---|---|---|
| 1 | 1 | nothing | yes, to start |
| 2 | 2 | nothing, for the database | yes, for the controller |
| 3 | 2 | any one | this is the one |
| 4 | 3 | any one | no better than three |
| 5 | 3 | any two | only across three or more sites |

So the path is one, then three, then five if you ever need it. A fourth machine adds capacity
to stand by and nothing at all to the quorum.

**Where they are decides whether any of this helps.** Three machines behind one uplink have a
quorum and no protection from that uplink failing. Worse, two machines at one site and one at
another means the site with two wins every vote: if that site is the one cut off from the
Forgejo nodes, it keeps the primary and the lease while the machine that can still reach the
nodes sits idle with no majority and a read-only database. Three failure domains, or at the
very least not all three behind the same link.

### Pointing the controllers at it

Name every server and say that only one of them will do:

```
postgres://forgesync:PASSWORD@db-a.example.org:5432,db-b.example.org:5432,db-c.example.org:5432/forgesync?sslmode=require&target_session_attrs=read-write
```

`target_session_attrs=read-write` is what makes a promotion enough on its own. Each connection
is offered to each server in turn and a standby refuses it, so after a promotion the
controllers find the new primary themselves: no proxy, no restart, no address to change. If
you would rather put something in front of them instead, name that one address and leave the
parameter off.

ForgeSync will not work in a standby whatever the connection string says. A standby answers
every read and accepts no write, so a controller that settled for one would report itself
healthy while nothing was being synced; instead it asks the server which it is
(`pg_is_in_recovery`) and refuses to start, and `/readyz` and `forgesync_database_up` say so
if it happens later.

### What a failover costs

Measured on the test environment, five Forgejo nodes, `controller.lease: 10s`, Patroni
`ttl: 20`. `deploy/test/check-db-failover.sh` runs the same check against a running
environment, so these are repeatable rather than reported.

| What happens | How long it takes |
|---|---|
| The primary is killed, and the controllers notice | under 1s (`/readyz` 503, `database_up` 0) |
| The leader stops acting, with nothing having taken over | 8 to 9s, inside the 10s lease |
| A standby is promoted and takes writes | 18 to 21s (Patroni's `ttl`) |
| A controller is leading again | 2 to 3s after that, so around 20 to 24s in all |
| A planned switchover (`patronictl switchover`) | inside 1s, without leadership dropping at all |
| Lost in either case | nothing: the same repositories, replicas and conflicts |

For a second or two after that, a write can still be refused with 409. The controller that
took the lease first hands it to the preferred one, and in between neither is leading. That
is the hand-over working; ask again.

The pages stayed up throughout both, and the killed server rejoined as a streaming replica
with nothing done to it by hand. The outage is Patroni's `ttl`, not ForgeSync's: shorten it
if you want a shorter one, and keep `controller.lease` below it, so a controller has always
stopped acting before anything else can start.

That gap is the whole point. What fences a leader off is its lease running out, measured on
its own clock, not either side noticing. There is therefore no moment at which two controllers
could both be acting, whatever the database is doing.

### If you would rather not

A managed PostgreSQL with failover is simpler still, and is someone else's pager. One machine
and a nightly dump is also a defensible answer: replication stops until somebody restores it,
nothing breaks, and nobody loses work on the nodes, because the nodes are where the work is.

## 13. Upgrades

Migrations run at startup under an advisory lock. Roll one controller at a time: stop it
(which gives the lease up, so the other takes over in about one renewal), put the new binary
in place, start it, watch `forgesync_leader` and the log, then do the other. Downgrades
aren't supported, because a migration that has run has run, so keep the dump from before.
