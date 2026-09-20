# Working on ForgeSync

## What you need

Go 1.27 or later, Node 22 or later, and Docker for the test environment. Git 2.32 or
later, because replication uses `--force-with-lease=<ref>:<expected>`.

## The commands

```sh
make check      # go vet, gofmt, the Go tests, the UI type-check and its tests
make test-db    # also the database tests, against the test environment's database
make build      # the UI, then bin/forgesyncd (UI embedded) and bin/forgesync
make run        # a controller against the local test environment
make web-dev    # Vite on :5173 with /api proxied to make run
```

`make check` is the gate every change has to pass. The database tests wipe the `public`
schema of whatever database they are given, so point `FORGESYNC_TEST_DATABASE_URL` at a
throwaway one and never at a running controller's. Two packages have such tests
(`internal/store` and `internal/leader`) and they share one database, so run them with
`-p 1`: in parallel, the store's `DROP SCHEMA` pulls the tables out from under the
leader's failover tests.

## The test environment

`deploy/test` brings up five Forgejo nodes, Keycloak standing in for SceneID, ForgeSync's
PostgreSQL, a webhook sink and one, two or three controllers:

```sh
cd deploy/test
./setup.sh --all              # everything, configured and ready
./setup.sh --standby          # a second controller on :8091, sharing the database
```

`PUBLIC_HOST=<host or IP>` makes it address everything by that name instead of the
`*.test` names, which is what a machine other than the host needs. It has to be one name
everywhere: OIDC compares the issuer in the token against the one the controller was
configured with, so the address people sign in at and the address the controller expects
must match.

The credentials in `deploy/test/.env` and in the realm JSON are throwaway and committed
on purpose, so the environment comes up without a setup ritual. Never reuse them
anywhere, and never put a real one there. Generated tokens land in `deploy/test/.tokens/`
and `phase0/.cache/`, both ignored by git.

## Before a release

`make check` is the gate; this is the slower pass, and `docs/SECURITY_REVIEW.md` is what
it produced last time.

```sh
golangci-lint run ./...                      # errcheck, govet, ineffassign, staticcheck, unused
govulncheck ./...                            # known vulnerabilities in what we import
gosec -exclude-dir=web ./...                 # security patterns in the Go source
gitleaks dir . --config .gitleaks.toml       # secrets, in the tree and in the history
osv-scanner scan source -r .                 # Go and npm dependencies
shellcheck -S warning $(git ls-files '*.sh') # the setup, probe and deploy scripts
cd web && npm audit                          # the UI's dependencies
```

`.golangci.yml` and `.gitleaks.toml` record which findings were looked at and
deliberately kept, and why. Keep those files honest rather than silencing a tool in
passing, and never widen an allowlist to cover a file: the one in `.gitleaks.toml` matches
a shape of string, so a real credential in the same file is still reported. `gosec`
reports five findings by design, listed in `README.md`.

## Conventions

- **Forgejo stays stock.** No patches, plugins or custom hooks, and no reading or writing
  Forgejo's database. The REST API, webhooks, the Git and LFS protocols, and the `forgejo`
  CLI in test setup only.
- **Never lose work, never decide for the owner.** No force-push over divergent history on
  ForgeSync's own judgement, no discarding conflicting metadata. What ForgeSync may do by
  itself is in `docs/ForgeSync_Solution_Architecture.md`; everything else becomes a
  conflict for a person.
- **Migrations are append-only.** Never edit an applied `internal/store/migrations/*.sql`;
  add the next one.
- **Every exported declaration has a doc comment.** `docs/CODE_REFERENCE.md` is the map,
  `go doc` is the detail, and the two stay in step.
- **Tests go beside the code, and the risky ones use the real thing.** Replication is
  tested against a real `git http-backend` rather than mocks, because the safety
  properties live in git's behaviour. Access control is a table in
  `internal/api/security_test.go`: a new write endpoint belongs in it.
- **Audit anything that changes something**, with an action name whose first word is its
  category, and give it a readable sentence in `web/src/eventText.ts`.
- **Shell scripts must run on macOS bash 3.2**: no associative arrays, no `mapfile`, no
  `${var,,}`, no GNU-only flags. `lib.sh` sets `set -Eeuo pipefail`, so a `grep` that finds
  nothing inside a pipeline aborts the script; wrap it as `{ grep ... || true; }`.
- **Check Forgejo's behaviour against the v16 source** before relying on it, at
  `https://codeberg.org/forgejo/forgejo/raw/branch/v16.0/forgejo/<path>`. Knowledge
  carried over from Gitea is often wrong in the details.

## Probes

`phase0/` holds the probes that establish what stock Forgejo does and does not allow.
`run-all.sh` runs them and writes a results file; `NODE_A=de NODE_B=uk ./run-all.sh` runs
them against another pair. Stop the controller first: it replicates and creates things,
which changes what the probes see, and `run-all.sh` refuses to start while a controller
answers on :8090.

Record a finding with `hyp "hypothesis" true|false "detail"` or `info`. A refuted
hypothesis is a design finding, not a bug in the probe. Suffix everything a probe creates
with `$RUN_ID` so runs are repeatable, and restore any shared configuration in an `EXIT`
trap.
