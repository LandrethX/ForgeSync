# Phase 0: what stock Forgejo supports

Executable checks of the assumptions in the ForgeSync architecture review. Each probe states
hypotheses and records whether they held, against the test environment in `../deploy/test`.

| Probe | Question |
|---|---|
| `p01-identity` | Does Forgejo store the SceneID `sub` where ForgeSync can find it? Does a user created in advance get linked on their first SceneID login, or duplicated? Is local sign-up blocked? |
| `p02-replica-protection` | Can a `*` branch rule and a `*` tag rule make a replica read-only for users while ForgeSync can still push? Do pushes with an expected old value fail safely? Can the owner remove the rule? |
| `p03-mirror-and-migration` | Are pull mirrors usable as replicas (read-only, sync on demand, convertible for promotion)? Does migrating between Forgejo instances keep issue numbers, authors and timestamps? |
| `p04-issues-sudo` | Does `sudo` keep authorship? Which timestamps can the API set? Do issue numbers collide across nodes? |
| `p05-webhooks` | Which changes send webhooks and which must be polled? Are deliveries signed? Can ForgeSync's own writes be recognised by sender? |
| `p06-pull-requests` | Can a PR merged on the primary be marked merged on a replica with the same merge commit (`manually-merged`, or automatic detection)? Can reviews be copied with `sudo`? Are `refs/pull/*` off limits? |
| `p07-ssh-keys` | Can ForgeSync copy SSH keys between nodes, even for users who haven't logged in there yet? Alternatively, can SceneID supply the keys in a login claim, with Forgejo syncing them at each login? |
| `p08-deprovisioning` | After a user is disabled in SceneID, what still works on the nodes? Can ForgeSync detect it and cut off tokens, sessions and SSH with `prohibit_login`? Is it reversible? |

## Running

Run this on the Mac, with the test environment already running (`../deploy/test/setup.sh`).
It needs `curl`, `jq`, `git`, `ssh-keygen` and `docker`. macOS 15 and later include `jq`; otherwise run `brew install jq`.

```sh
./run-all.sh            # every probe; takes a few minutes, mostly waiting for webhooks
./run-all.sh p02 p05    # selected probes
```

The report is written to `results/run-<id>.md`, with raw rows in `results/run-<id>.tsv`.
Each run creates new repositories and users, suffixed with the run id, so runs don't interfere.

Verdicts:

- **CONFIRMED** or **REFUTED**: whether the stated hypothesis held. REFUTED isn't a failure;
  it means the design has to change.
- **INFO**: an observed fact.
- **ERROR**: the probe itself broke. Check the console output.

## Notes

- `lib.sh` drives the SceneID browser login with curl: it fetches the Keycloak login form,
  posts the credentials and follows the redirects back to Forgejo. If Keycloak changes its
  login page, `sceneid_login` is the place to fix.
- `p07` changes SceneID's realm (allows custom user attributes, adds an `ssh_public_keys` claim)
  and sets the SSH key attribute on DK's login source while it runs. The login source is reset
  when the probe exits; the realm changes stay until `sceneid` is recreated.
- API tokens for SceneID users are made with the Forgejo CLI and cached in `.cache/`.
  Stale tokens are replaced automatically after the environment is reset.
