# What ForgeSync costs at scale

Measured on one Debian LXC running five Forgejo nodes and two controllers in Docker,
with 203 repositories on every node, one commit each. `deploy/test/scale.sh make 200`
creates them and `scale.sh drop` takes them away again, so the measurement can be
repeated.

| What | Measured |
|---|---|
| A push to one repository reaching all four replicas (webhook fast path) | **5s** |
| Scanning one node's 203 repositories | 3-4s (the five nodes are scanned in parallel) |
| A full round: scan, primaries, conflicts, replication, issues | **3m 7s** |
| ... of which replication | 2m 56s |
| ... primaries, conflicts | 5ms, 9ms |
| ... issue replication | 11s |
| First replication of 200 new repositories (800 replicas), from scratch | ~6 min, no errors |
| Controller memory | 100 MB leading, 7 MB standing by |
| ForgeSync's database | 12 MB for 203 repositories, 812 replicas, 832 refs |
| The git cache under `replication.work_dir` | 51 MB |

What that says. **The fast path is what people feel, and it stays fast**: a push is
replicated in seconds whatever the repository count, because a webhook replicates one
repository. The full round is the safety net and it grows with repositories times nodes --
at 200 repositories it's about three minutes, so the interval has to stay comfortably
above it (the test environment uses 30m). Replication dominates it, and raising
`replication.concurrency` from 2 to 8 changed nothing, so at this size the limit is what
the Forgejo nodes can serve, not ForgeSync's parallelism. Primaries and conflict detection
are nothing: they're SQL and a compare call per changed repository. The round logs each
part at debug level (`scan round part finished`), so when a round outgrows the interval
the log says which part to look at.

A scan taken *while* a large replication run is going costs much more (4 minutes for one
node, against 4 seconds idle): the contention is on the nodes, not in the controller.

These numbers are from a test environment, not from production, and nothing has been
measured with large pushes or deep history. See `docs/LIMITATIONS.md`.

## And at 1000 repositories

Measured on 2026-09-21 on the same server, with `scale.sh make 1000` on SE and the other
four nodes empty, so this is also the worst case: every repository had to be created and
filled on four replicas at once.

| What | Measured |
|---|---|
| Creating 1000 repositories through the API | about 4 min |
| Scanning one node's 1003 repositories, once every node has them | **19s** (3-4s for 203) |
| First replication of 1000 new repositories (4000 replicas), from scratch | **29.5 min**, no errors |
| The round that follows it, putting the guard on 4000 new replicas | **17m 18s** |
| A settled round, with nothing at all to write | **17m 24s** |
| A push reaching all four replicas **while that run was going** | **11s** |
| Controller memory | **300 MB** at the peak of a round, 6 MB standing by |
| ForgeSync's database | 16 MB |
| The git cache under `replication.work_dir` | 197 MB |

What changed and what did not. **Replication is linear in repositories times nodes**: five
times the repositories took 4.9 times as long, at a steady 33 repositories a minute per
replica throughout. Nothing degraded as the numbers grew.

**The scan is linear too**, and it is worth saying how that was got wrong here first: SE's
very first scan of 1003 repositories took 4.6s, which looked flat against 3-4s for 203. It
was not. At that moment only SE held the repositories. Once all five nodes held 1003 each
and were scanned in parallel, a per-node scan settled at **19s**, which is about five times
203's for five times the repositories. A scan is a page of fifty at a time plus one read of
each default branch head, and neither gets cheaper.

**The fast path stays a fast path under load**, which is the property that matters most.
The 11s above was measured with the engine mid-way through replicating a thousand
repositories; at 203 idle it was 5s. A webhook replicates one repository, and it does not
queue behind the bulk run.

**A settled round still costs 17 minutes.** This is the number to plan from, and it is not
the writing: the round above wrote nothing at all. It is the looking. Comparing a thousand
repositories against four replicas each means fetching and comparing four thousand times,
and that happens whether or not anything has changed. So `inventory.interval` has to stay
above it, and at this size the webhooks are not an optimisation but the mechanism: the round
is the safety net that catches what a lost delivery missed.

**Memory is 300 MB at the peak of a round**, against 100 MB at 203 repositories. It is the
round's working set rather than anything kept, and it goes back down; a standby still sits
at 6 MB. This is the number that decides how small a container can be. A `--binary` install
with no nodes yet sits at **52 MiB of 512 on a real Proxmox LXC**, so 512 MB is generous for
a first machine; it is a thousand repositories that argues for a gigabyte, not the install.
`deploy/prod/README.md` section 1 has both. The database is small either way: what ForgeSync keeps per repository is a row,
its replicas and its refs.

The one thing to watch is **disk**, and it is the git cache: 197 MB for 1000 tiny
repositories, so plan from the size of your own repositories rather than from this number.

**A bulk import costs two rounds, not one.** `protectReplicas` acts on the nodes that
`hasRepo` says have the repository, and that comes from the replica rows, which the *scan*
writes. So a replica created by replication is guarded on the round after the scan records
it. Here that was a second round of **17m 18s** doing nothing but putting the guard on 4000
new replicas, one API call each. It happens once, and only for replicas that are new.

What a round costs at this size is therefore dominated by replication and by whatever runs
per replica rather than per repository. Turning off what an installation does not need is
the lever, and `/api/v1/overview` lists what this controller has on.
