# What ForgeSync costs at scale

Measured on one Debian LXC running five Forgejo nodes and both controllers in Docker,
with 203 repositories on every node, one commit each. `deploy/test/scale.sh make 200`
creates them and `scale.sh drop` takes them away again, so the measurement can be
repeated.

| | |
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
measured at thousands of repositories or with large pushes and deep history. See
`docs/LIMITATIONS.md`.
