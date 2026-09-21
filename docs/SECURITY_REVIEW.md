# Secure code quality gate: ForgeSync v0.9.0

A full review of the codebase before its first publication, against the risk-based
secure code quality standard this project holds itself to: testing depth chosen from the
exposure, data, privilege and blast radius of the code, with checks drawn from the OWASP
Top 10 and ASVS, the CWE Top 25, and the NIST Secure Software Development Framework. Run
on 2026-09-20 against commit `335b066` plus the changes this review produced.

## Classification

| | |
|---|---|
| **Technology** | Go 1.27 (chi, pgx, cobra), React 19 + TypeScript + Vite, PostgreSQL 18, Alpine-based container, Docker Compose test environment |
| **Scope** | Whole codebase, not a diff: a first review before publication, so the system is classified by its highest exposure rather than by the most recent change |
| **Change under review** | Every tracked file, including build, deployment, CI and script files |
| **Exposure** | Internet-facing HTTP API and web application, plus webhook endpoints reachable by the Forgejo nodes |
| **Data** | Site-admin API tokens for every node, account passwords, session material, an audit trail |
| **Privilege** | A controller is the most privileged thing in a Forgejo installation: site-admin on every node, and able to act as any user |
| **Level** | **LEVEL 4, critical security code.** Authentication, authorisation, sessions, password storage, token handling, administrative interface and a security control (branch protection) are all in scope. |

Level 4 is what the escalation rules give: the automatic triggers for login,
authorisation, password storage, token validation and administrative security functions
all apply, and where a change touches several categories the highest one counts.

## Quality

| Check | Tool | Result |
|---|---|---|
| Formatting | `gofmt -l` | **PASS**, no files |
| Static checks | `go vet ./...` | **PASS** |
| Lint | `golangci-lint run ./...` (errcheck, govet, ineffassign, staticcheck, unused) | **PASS**, 0 issues |
| Types (UI) | `tsc --noEmit` | **PASS** |
| Unit and integration tests | `go test ./...` | **PASS**, 13 packages |
| Database tests | `go test -p 1 ./internal/store ./internal/leader` against PostgreSQL | **PASS** |
| UI tests | `vitest run` | **PASS**, 75 of 75 in 13 files |
| Race detection | `go test -race` | **NOT RUN**. Reason: the review host has no C compiler and the detector needs cgo. Needs: any build host with a toolchain. Owner: whoever runs the release build. It is not part of `make check` either, so no run has ever covered it, and the controller is concurrent by design. |
| Documentation | doc comments on exported declarations | **PASS** after this review: 467 of 467, up from 429 |

## Security

| Check | Tool | Result |
|---|---|---|
| SAST | `gosec ./...` | **PASS with findings**: 16, all reviewed. 5 medium are by design (below), 11 low are `G104` on writes whose error has nobody to report to. |
| SAST, second opinion | `semgrep p/golang p/javascript p/security-audit p/secrets` | **PASS with findings**: 3, all reviewed as false positives (below). |
| Dependency vulnerabilities | `govulncheck ./...` | **PASS**, none |
| Dependency vulnerabilities | `osv-scanner scan source -r .` | **PASS**, none across 14 Go and 142 npm packages |
| Secrets, working tree | `gitleaks dir .` | **PASS** after triage: 96 findings, every one a probe-generated username or a commit hash in `phase0/results`. `.gitleaks.toml` records why, matching the string shape rather than exempting the files. |
| Secrets, history | `gitleaks git .` | **PASS**, 70 commits, same findings and same triage |
| Shell | `shellcheck -S warning` over 14 scripts | **PASS** |
| Dockerfile | `hadolint` | **PASS with findings**: DL3066 fixed, DL3018 accepted |
| IaC and configuration | `trivy config .` | **PASS**: one low (no HEALTHCHECK), fixed |
| Container image | `trivy image forgesync:test` | **PASS**, no vulnerabilities (Alpine 3.23.6) |
| DAST | `zap-baseline.py` against a running controller | **PASS**: 0 failures, 65 passes, 2 informational warnings |
| Authentication | Tests plus manual review | **PASS** |
| Authorisation | `internal/api/security_test.go`, new in this review | **PASS**: every write endpoint against nobody signed in, each role below the one it needs, and the role itself |
| CSRF | Test over every write endpoint | **PASS** |
| Injection | Manual review of every SQL statement | **PASS**: every value is a placeholder; the only string building is placeholder names, and `ILIKE` input is escaped |
| Cryptography | Manual review | **PASS**: PBKDF2-HMAC-SHA256 at 600,000 iterations, 16-byte salt, constant-time compare; session keys from `crypto/rand`, stored only as SHA-256; webhook HMAC compared with `hmac.Equal`; TLS 1.2 minimum |
| Error handling and logging | Manual review | **PASS**: no secret is logged, and a server error answers with a generic message while the detail goes to the log |
| Fuzzing | Go native fuzzing | **NOT RUN**. Reason: nothing parses untrusted binary input. The only outside data parsed is a JSON webhook body behind an HMAC, and git's own `--porcelain` output. Needs: a parser worth fuzzing. Owner: revisit when ForgeSync parses a format itself. |
| Tenant isolation | Not applicable | ForgeSync has no tenants and no per-user resources: access is by role alone, so BOLA and IDOR have no object to apply to. |

## What ran

| Tool | Version | Command |
|---|---|---|
| gosec | v2.29.0 | `gosec -quiet -exclude-dir=web ./...` |
| semgrep | 1.176.1 | `semgrep scan --config=p/golang --config=p/javascript --config=p/security-audit --config=p/secrets` |
| govulncheck | v1.8.0 (golang.org/x/vuln) | `govulncheck ./...` |
| osv-scanner | v2.6.0 | `osv-scanner scan source -r .` |
| gitleaks | v8.30.1 | `gitleaks dir .` and `gitleaks git .`, with `--config .gitleaks.toml` |
| golangci-lint | v2.13.2 | `golangci-lint run ./...` |
| staticcheck | 2026.2.1 | through golangci-lint |
| shellcheck | 0.11.0 | `shellcheck -S warning $(git ls-files '*.sh')` |
| hadolint | 2.15.1 | `hadolint Dockerfile` |
| trivy | 0.74.0 | `trivy config .` and `trivy image forgesync:test` |
| ZAP | zap-stable (2026-09) | `zap-baseline.py -t http://<controller>:8090 -I` |
| Go | 1.27.1 | `go vet`, `go test`, `gofmt` |

Two of these report no version of their own when installed from source, because their
release process is what stamps one in; the versions above come from `go version -m`.

Every fix below was re-tested by the check that found it, after rebuilding and
redeploying, not in the source alone.

## Findings

### Fixed in this review

**MEDIUM: behind a reverse proxy, one client could lock out every administrator.**
The sign-in limiter counted failures against `RemoteAddr`, and the audit trail recorded
the same. With a proxy in front, which `deploy/prod/README.md` documents as a supported
deployment, every request appears to come from the proxy: ten wrong passwords from anyone
would have blocked sign-ins for everybody for five minutes, and every sign-in in the
history would have named the proxy rather than the person.
*Fix:* `http.trusted_proxies` names the proxies whose `X-Forwarded-For` is believed.
`Server.clientAddr` reads the chain from the right and takes the first address that is not
one of ours, so a client's own claim about its address proves nothing. Empty by default,
which keeps the old behaviour. Eleven table cases in `internal/api/security_test.go`,
including a client that cannot lock out another behind the same proxy.

**LOW: no HSTS on a controller serving TLS itself.** `http.tls_cert_file` makes the
controller serve browsers directly, with no proxy to add the header.
*Fix:* `Strict-Transport-Security: max-age=31536000` on requests that arrived over TLS,
and only those. A plain listener may be the one a proxy talks to, and a browser told that
`http://host` is HTTPS-only cannot be told otherwise for a year.

**LOW: missing isolation and feature-policy headers.** ZAP reported no
`Cross-Origin-Embedder-Policy`, and no `Permissions-Policy`.
*Fix:* `Cross-Origin-Opener-Policy: same-origin`, `Cross-Origin-Resource-Policy:
same-origin`, `Cross-Origin-Embedder-Policy: require-corp`, and a `Permissions-Policy`
that turns off the device APIs the UI never asks for. Everything the page loads is its
own, which the content security policy already required, so requiring each resource to say
so costs nothing. Re-scanned after deploying: the warnings are gone and the UI still loads.

**LOW: container image hardening.** `USER forgesync` by name rather than number, and no
`HEALTHCHECK`.
*Fix:* `USER 10001`, and a healthcheck on `/readyz` matching what the compose file and
systemd already watch.

### Accepted, with the reason

**`gosec`, five medium findings.** Two `G124` on the session cookie, because `Secure` is a
setting rather than a literal (it has to be, for a reverse proxy terminating TLS, and the
cookie is HttpOnly and SameSite=Strict in every case). One `G204` on the git CLI, whose
arguments ForgeSync builds and which is never passed through a shell. Two `G304` on
reading the config and token files, from paths the config gives. `README.md` says the
same.

**`gosec`, eleven low `G104` findings.** Unchecked errors on writes to an
`http.ResponseWriter` or to a temporary file being discarded. `.golangci.yml` records the
reasoning: a handler that has already sent a status has nobody left to tell.

**`semgrep`, three findings.** Two are the same cookie pattern as `G124`. The third is
`filepath-clean-misuse` in `internal/webui/webui.go`, where `path.Clean` is used on a URL
path before looking a file up. It is not a traversal: the lookup is against an `embed.FS`
that contains only the built UI, `fs.Stat` rejects a path containing `..` outside it, and
`http.FileServer` cleans again. Nothing on the host filesystem is reachable.

**`hadolint` DL3018, unpinned `apk add`.** Pinning Alpine package versions makes the build
fail when a version ages out of the repository, which is a worse failure than the one it
prevents; the base image is pinned to `alpine:3.23` and `trivy` scans the result.

**INFORMATIONAL: the sign-in limiter is per controller, in memory.** Each controller allows
its own window of attempts before a block, so N of them mean N times the attempts, and a
failover clears the window. With a 12-character
minimum and PBKDF2 at 600,000 iterations the remaining exposure is small, and moving the
counter into the shared database would make the database a dependency of refusing a
sign-in. Left as it is, deliberately.

**INFORMATIONAL: one goroutine per accepted webhook delivery.** A node that floods
deliveries could start many, but a delivery must carry a valid HMAC for that node, and
`Engine.Trigger` coalesces repeated work for the same repository. Not worth a queue yet.

**INFORMATIONAL: `VerifyPassword` takes the iteration count from the stored hash.** That
is what lets the cost be raised without invalidating existing passwords; the value comes
from ForgeSync's own database, and a corrupted row costs one slow comparison.

## What the limitations document already says

Not findings of this review, but the honest context for it: PostgreSQL is a single point
of failure, nothing has been measured past 200 repositories, Actions secrets and the
package types whose registries need their own client cannot be replicated at all, and
ForgeSync has not yet run in production. See `docs/LIMITATIONS.md`.

## Publication readiness

Run before the repository is made public, since publishing exposes every commit rather than
the current tree.

| Check | Result |
|---|---|
| Secrets over the full history | **PASS**, 71 commits, no findings |
| Committed credentials are throwaway | **PASS**: `deploy/test/.env` and the realm JSON hold test-environment credentials, committed on purpose so the environment comes up without a ritual, and `CONTRIBUTING.md` says never to reuse them |
| Internal addresses and topology | **PASS** after a change: the lab machine's address appeared in a script comment and a UI test fixture, and is now the documentation range `192.0.2.10`. The `*.test` hostnames are the compose network's own and are meant to be read |
| Personal data, including commit metadata | **REVIEWED**: the history carries one author identity, which is the author's own and is published deliberately. Nothing else personal is in the tree |
| Customer, vendor and ticket references | **PASS**, none |
| Licence and copyright holder | **PASS**: Apache-2.0, Copyright 2026 Landreth, with `NOTICE` |
| Third-party and vendored code | **PASS**: no vendored source. Dependencies are permissive (MIT, Apache-2.0, BSD-3), and no Forgejo code is included or linked |
| Dependency and image scans current | **PASS**, against what is published |
| Documentation honest about what is untested | **PASS**: `docs/LIMITATIONS.md` and `docs/PERFORMANCE.md` |
| Security contact stated | **PASS**: `SECURITY.md` |
| History rewrite verified | **PASS**: tree hash identical before and after, tag `v0.9.0` preserved, old refs cleared |

## Result

**PASS with non-blocking findings.**

| Severity | Open | Fixed here |
|---|---|---|
| Critical | 0 | 0 |
| High | 0 | 0 |
| Medium | 0 | 1 |
| Low | 0 | 3 |
| Informational | 3, accepted with reasons above | 0 |

Two checks are **NOT RUN** rather than passed: race detection, for want of a C compiler on
the review host, and fuzzing, for want of anything to fuzz. Neither is recorded as a pass.

No test or security control was weakened to reach this result.

The one allowlist added (`.gitleaks.toml`) was verified rather than trusted. Its first
version exempted the `phase0/results` paths, and a planted Forgejo token and a planted
GitHub token in one of those files both went unreported: the scan still ran and still
passed, and had stopped looking. It was rewritten to match the shape of the probe-generated
usernames instead of the files that hold them, and re-tested: the planted Forgejo token is
reported by the repository's own rule, the planted GitHub token by the default rules, and
the real tree is still clean.

---

# Gate: the database failover change

Run on 2026-09-20 against the working tree on top of commit `5d203e7`, before committing
and before the first push to a public remote.

## Classification

| | |
|---|---|
| **Scope** | The uncommitted diff, plus the publication-readiness checks over the whole tree and the whole history, because the repository is about to be published |
| **Change under review** | `store.Ping` asks the server whether it is a standby (`pg_is_in_recovery`) and `Open` refuses one; every database read in the metrics handler shares one short budget; a test-only PostgreSQL-under-Patroni image and compose profile; an end-to-end failover check; documentation |
| **Exposure** | Authenticated HTTP endpoints (`/metrics`, `/readyz`, `/api/v1/overview`) on an internet-facing controller; the new container and compose services are test-environment only |
| **Data** | A database connection string; no new handling of tokens, passwords or session material |
| **Privilege** | Unchanged. Nothing touches authentication, authorisation, sessions, cryptography or secret storage |
| **Level** | **LEVEL 3, exposed code.** The escalation rules for authentication, authorisation, cryptography and secrets do not fire; the changed code is reached through an exposed API, and Docker and Compose definitions changed, so the container and IaC checks apply. |

## Result

| Check | Tool | Result |
|---|---|---|
| Formatting | `gofmt -l` | **PASS**, no files |
| Static checks | `go vet ./...` | **PASS** |
| Lint | `golangci-lint run` | **PASS**, 0 issues |
| Types (UI) | `tsc --noEmit` | **PASS** |
| Tests | `go test ./...` | **PASS**, 13 packages |
| Database tests | `go test -p 1 ./internal/store ./internal/leader` | **PASS** |
| UI tests | `vitest run` | **PASS**, 75 of 75 |
| Behaviour, end to end | `deploy/test/check-db-failover.sh` | **PASS** against the five live nodes: a killed primary and a planned switchover, nothing lost either time |
| SAST | `gosec` | **PASS with findings**: the same 16 as the full review, none in the changed code |
| SAST, second opinion | `semgrep p/golang p/security-audit` | **PASS with findings**: the same 3, none in the changed code |
| Dependency vulnerabilities | `govulncheck ./...` | **PASS**, none |
| Secrets, working tree | `gitleaks dir .` | **PASS**, no leaks |
| Secrets, history | `gitleaks git .` | **PASS**, 69 commits, no leaks |
| Allowlist verification | planted credential | **PASS**, reported by the repository's own rule |
| Shell | `shellcheck` at default severity | **PASS**, 0 findings in the new scripts |
| Dockerfile | `hadolint` | **PASS with findings**: DL3008 accepted, as DL3018 already is |
| IaC and configuration | `trivy config deploy/test` | **PASS after remediation**: one low fixed, one high reviewed as a false positive |
| Container image, product | `trivy image forgesync:test` | **PASS**, no vulnerabilities |
| Container image, test-only | `trivy image forgesync-patroni:test` | **PASS with findings**: inherited from `postgres:18`, see below |
| DAST | `zap-baseline.py` against the running controller | **PASS**: 0 failures, 65 passes, 2 informational |
| Race detection | `go test -race` | **NOT RUN**. Reason: still no C compiler on this host. Needs: a build host with a toolchain. Owner: whoever runs the release build. Second consecutive review with this gap, and the controller is concurrent by design. |

## Findings

**The test-only Patroni image carries the vulnerabilities of its base.**
`forgesync-patroni:test` is built from `postgres:18` (Debian 13) and reports 1 critical and
80 high in the operating system layer, plus 1 critical and 21 high in `gosu`, a Go binary
the official PostgreSQL image ships. Two things bound it. It is **test-environment only**:
`deploy/test` builds it for `check-db-failover.sh`, it is never published, never shipped and
never runs anywhere but a throwaway compose network. And the same `gosu` findings are
already present in `postgres:18-alpine`, which the test environment used before this change,
so the delta is the Debian package layer. The two criticals are a `libxml2` denial of service
with no fix available upstream, in an image that parses no untrusted XML, and a TLS session
resumption flaw in `gosu`, which makes no TLS connections and exists only to drop privileges
at startup. The product image, `forgesync:test`, still reports nothing at all. Accepted as
test-only; moving it to an Alpine base would need Patroni's PostgreSQL driver built against
musl, and would buy nothing outside the test environment.

**`trivy` DS-0002, no `USER` in the Patroni Dockerfile.** A false positive, verified rather
than argued: the entrypoint starts as root only to take ownership of the data volume and
then `exec gosu postgres patroni`, which is what the official PostgreSQL image does.
`gosu postgres id` in the built image reports uid 999, and the long-running process is that
user. A static scanner cannot see the drop. Documented in the Dockerfile itself.

**`trivy` DS-0026, no `HEALTHCHECK`. Fixed and re-tested.** A `HEALTHCHECK` running
`pg_isready` was added, the image rebuilt, and `trivy config` re-run: 26 of 27 tests pass
where 25 did, and the finding is gone. `docker inspect` confirms the healthcheck is in the
built image.

**`hadolint` DL3008, unpinned `apt-get install`.** Accepted for the same reason DL3018 is
accepted on the product image: pinning Debian package versions makes the build fail when a
version ages out of the archive, which is a worse failure than the one it prevents. The base
image and the Patroni version are both pinned, and the result is scanned.

## Publication readiness

Repeated here because this is the last gate before the repository becomes public.

- Secret scanning covered the working tree **and all 69 commits**: no leaks.
- The allowlist was verified by planting a Forgejo-token-shaped credential in a file the
  allowlist covers. It was reported. The allowlist matches a string shape, not a path.
- The credentials committed on purpose (`deploy/test/.env`, the realm JSON) are throwaway
  test values and the file says so. This change adds one more,
  `FORGESYNC_DB_REPLICATION_PASSWORD`, in the same file under the same warning.
- No internal hostname or address is in the tracked tree; the test environment is addressed
  through `PUBLIC_HOST`.
- Licence, `NOTICE` and the security contact are unchanged and present.
- `docs/LIMITATIONS.md` was updated by this change and remains honest about what has not
  been run.

---

# Gate: node credentials in the database

Run on 2026-09-20 against the working tree on top of commit `878c720`, before committing.

## Classification

| | |
|---|---|
| **Scope** | The uncommitted diff |
| **Change under review** | `internal/secret` (AES-256-GCM sealing of node tokens), `internal/nodes` (which nodes an installation has), migration 0033, `store.Nodes/SaveNode/RetireNode`, the `node_key_file` config option, and the startup path that uses them |
| **Exposure** | Not reachable from the network: this is startup and storage. The admin API and UI that will let somebody add a node are not in this change |
| **Data** | **Site-admin API tokens for every Forgejo node**, and the key that seals them |
| **Privilege** | A node token is site-admin on that Forgejo; the key opens every one of them |
| **Level** | **LEVEL 4, critical security code.** The escalation rules fire on cryptography, credential storage and secrets management. |

## Result

| Check | Tool | Result |
|---|---|---|
| Formatting | `gofmt -l` | **PASS** |
| Static checks | `go vet ./...` | **PASS** |
| Lint | `golangci-lint run` | **PASS**, 0 issues |
| Tests | `go test ./...` | **PASS**, 15 packages (2 new) |
| Database tests | `go test -p 1 ./internal/store ./internal/leader` | **PASS** |
| New behaviour | `internal/secret`, `internal/nodes` tests | **PASS**: round trip, fresh nonce per seal, wrong key refused, tampering and truncation refused, key size enforced, 0600 on generate, refusal to overwrite a key, hex and base64 accepted; and for resolution, config carried across, sealed node needs no config entry, every unusable node an error rather than an omission |
| Behaviour, end to end | five live nodes, three controllers | **PASS**: migration applied, all five nodes carried across unsealed, all healthy, 3 repositories and 12 replicas unchanged |
| SAST | `gosec` | **PASS with findings**: 18, the baseline 16 plus 2 of an already-accepted class (below) |
| SAST, second opinion | `semgrep p/golang p/security-audit p/secrets` | **PASS**: the same 3 as the baseline, none in the new code, and nothing from the crypto or secrets rules |
| Dependency vulnerabilities | `govulncheck ./...` | **PASS**, none. `go.mod` is unchanged: the sealing is standard library only, so this adds no supply chain at all |
| Secrets, working tree | `gitleaks dir .` | **PASS**, no leaks |
| Secrets, history | `gitleaks git .` | **PASS**, no leaks |
| Cryptographic review | by hand, below | **PASS** |
| DAST | not run | **NOT APPLICABLE**: no HTTP surface changed. It applies to the next change, which adds the endpoints |
| Race detection | `go test -race` | **NOT RUN**. Reason: still no C compiler on this host. Needs: a build host with a toolchain. Owner: whoever runs the release build. Third consecutive review with this gap |

## Cryptographic review

Taken deliberately, because a scanner passing is not the same as the construction being right.

- **Algorithm**: AES-256-GCM through `crypto/cipher`, an established AEAD. Nothing invented.
- **Key**: 32 bytes, enforced, from `crypto/rand`. `GenerateKey` writes 0600 and refuses to
  overwrite an existing key, because replacing one silently would leave every sealed token
  unopenable.
- **Nonce**: 12 bytes from `crypto/rand`, fresh for every seal, carried in front of the
  ciphertext in the standard Go idiom. Reuse is what breaks GCM, and random 96-bit nonces
  are safe well past any number of seals this can perform: a node's token is sealed when the
  node is added and when its token is replaced, so a busy installation might do it dozens of
  times in its life.
- **Authentication**: GCM authenticates, and the tests confirm a flipped byte at the front,
  the middle and the end, and a truncated value, are all refused rather than decrypted.
- **Errors**: one `ErrWrongKey` for a wrong key, a truncated value and a tampered one. The
  caller can do nothing different about any of them, and telling them apart would be an
  oracle.
- **Additional data**: none. Binding a sealed token to its node's name was considered and
  declined: it would stop somebody with write access to the database moving a token from one
  node's row to another's, but that same access can simply point a node at a server they
  control, which gets them the token either way. The mitigation is that ForgeSync's database
  is a trusted component, which is why the sealing is aimed at *copies* of it (dumps,
  backups, standbys, verification restores) and not at somebody who has the live one.
- **Concurrency**: `cipher.AEAD` is safe for concurrent use, which is what the type's
  documentation promises and what the comment on `Key` claims.

## Findings

**`gosec` G304 x2, reading the node key from a path in the config.** Accepted, and the same
class the baseline already accepts for the config and token files: "from paths the config
gives". The path comes from `node_key_file`, which only whoever writes the config file can
set, and that is already the most privileged thing about the installation.

**Nothing else new.** The other 16 `gosec` findings and all 3 `semgrep` findings are the
baseline's, unchanged and none in the new code.

## What this change deliberately does not do yet

The admin API and UI that let somebody add a node are not here, and neither is picking a new
node up without a restart. Until they are, `node_key_file` is optional and changes nothing:
an installation that does not set it keeps its nodes in the config file with their tokens in
files, which is what every existing installation does and what the end-to-end run confirmed.

---

# Gate: adding nodes, and the uplink check

Run on 2026-09-20 against the working tree on top of commit `89a5ff3`, before committing.

## Classification

| | |
|---|---|
| **Scope** | The uncommitted diff |
| **Change under review** | `nodes.Admin` and three admin endpoints for adding, checking and retiring a node; the add-node wizard in the UI; `nodes.Watch` so a change is picked up; `health` uplink probing and the suppression that goes with it; `store.RetiredNames`; two config settings |
| **Exposure** | **Internet-facing HTTP API.** Three new write endpoints, one of which takes a site-admin token for another machine and stores it |
| **Data** | A Forgejo node's API token, which is site-admin on that node |
| **Privilege** | Administrator. Adding a node decides what ForgeSync will act on |
| **Level** | **LEVEL 4, critical security code.** Credential handling and an administrative function, reached over the network. |

## Result

| Check | Tool | Result |
|---|---|---|
| Formatting | `gofmt -l` | **PASS** |
| Static checks | `go vet ./...` | **PASS** |
| Lint | `golangci-lint run` | **PASS**, 0 issues |
| Types (UI) | `tsc --noEmit` | **PASS** |
| Tests | `go test ./...` | **PASS**, 17 packages |
| Database tests | `go test -p 1 ./internal/store ./internal/leader` | **PASS** |
| UI tests | `vitest run` | **PASS**, 80 of 80 in 14 files (5 new for the wizard) |
| Authorisation | `internal/api/security_test.go` | **PASS**: the three new endpoints were added to the walk, so each is tried signed out and at every role below Administrator |
| Behaviour, end to end | five live Forgejo nodes | **PASS**: a real node checked and added with its token sealed, a bad token refused with the reasons and nothing stored, a node retired and followed by the controller in 12s, added back and in use 6s later |
| SAST | `gosec` | **PASS with findings**: 18, unchanged from the last gate, **none in the new code** |
| SAST, second opinion | `semgrep p/golang p/security-audit p/secrets` | **PASS**: the same 3 as the baseline, none in the new code |
| Dependency vulnerabilities | `govulncheck ./...` | **PASS**, none; `go.mod` unchanged |
| Secrets, working tree | `gitleaks dir .` | **PASS** |
| Secrets, history | `gitleaks git .` | **PASS** |
| Shell | `shellcheck -S warning` | **PASS** |
| DAST | `zap-baseline.py` against the running controller | **PASS**: 0 failures, 65 passes, 2 informational, unchanged |
| Race detection | `go test -race` | **NOT RUN**. Reason: still no C compiler on this host. Needs: a build host with a toolchain. Owner: whoever runs the release build. **Fourth consecutive review with this gap**, on code that is concurrent by design and has just gained another goroutine |

## How the token is handled

The one new secret path, taken deliberately.

- It arrives over the admin API, which needs Administrator and, from a browser, the CSRF
  header. It is read from the body and never logged.
- It is **sealed before it is stored** (`internal/secret`, AES-256-GCM under the node key),
  so the database holds ciphertext, as for a node added any other way.
- It is **never returned**. `Check` and `Add` answer with findings, the node's version and
  who the token turned out to belong to; the token itself appears in no response.
- The audit entry records that a node was added and by whom, with the address, the site, the
  service user and the SceneID source id. It does **not** record the token, which a test
  asserts by looking for it in the audited details.
- Nothing is stored when a blocking check fails, so a token given to the wrong address is not
  kept because the request otherwise looked well formed.
- Without a node key the endpoints refuse and say which setting to add, rather than storing
  the token in the clear.

## Findings

**None new.** The 18 `gosec` and 3 `semgrep` findings are the baseline's, in code this change
did not touch.

**A design fault found by testing rather than by reading**, worth recording because it was
invisible in the code: retiring a node that a controller's config file still listed took it
straight back in on the next resolve, so the button appeared to do nothing. `Resolve` now
asks `store.RetiredNames` and leaves those out, and says in the log that the file names a
node that has been retired. There is a test for it.

## What this deliberately does not do

ForgeSync does not configure the node. The `app.ini` keys it needs (`ALLOWED_HOST_LIST`,
`LFS_START_SERVER`) cannot be reached through any API and need Forgejo restarted, so the
wizard says what to do and then checks what it can see. That is the whole shape of it:
instructions, then verify, and nothing done on the node's behalf with a token somebody may
not have meant to hand over.

---

# Gate: the three-machine database, and closing the race gap

Run on 2026-09-21 against the working tree on top of commit `8992fea`, before committing.

## Classification

| | |
|---|---|
| **Scope** | The uncommitted diff |
| **Change under review** | `deploy/prod/cluster.sh`, which installs etcd and Patroni and hands a live PostgreSQL to them; a flaky test fixed; `make test-race` added to `make check`; documentation |
| **Exposure** | Not network-reachable code. A root script on the database machines, and a test change |
| **Data** | The whole of ForgeSync's state: it adopts the running database. It also writes the database and replication passwords into a Patroni config and a bundle |
| **Privilege** | Root, and it takes ownership of PostgreSQL away from Debian's own unit |
| **Level** | **LEVEL 4.** A root script that moves a live database and handles credentials. |

## Result

| Check | Tool | Result |
|---|---|---|
| Formatting | `gofmt -l` | **PASS** |
| Static checks | `go vet ./...` | **PASS** |
| Tests | `go test ./...` | **PASS** |
| **Race detection** | `CGO_ENABLED=1 go test -race ./...` | **PASS**, including the database tests. See below: this closes a gap that had been NOT RUN for four reviews |
| Types and UI | `tsc --noEmit`, `vitest run` | **PASS**, 80 of 80 |
| Shell | `shellcheck` at default severity | **PASS**, including the new script |
| Secrets | `gitleaks dir .` | **PASS** |
| Behaviour, end to end | three Debian 13 machines running systemd | **PASS**, below |

## Race detection, finally run

It had been reported `NOT RUN` in four consecutive gates, for want of a C compiler on this
host. That was a fixable environment problem rather than a real obstacle: `apt-get install
gcc`, and it runs. The whole suite passes under `-race`, including the database tests, which
is worth having on a controller that runs a health monitor per node, a lease, a replication
engine and now a node watcher.

`make test-race` is part of `make check` so it stays run rather than becoming a thing
somebody remembers.

It found no race. It did find a **flaky test**: `TestMonitorRunDetectsOutageAndRecovery`
asserted an exact transition sequence that assumed the first health check finished inside
10ms, which is not true on a loaded machine and was never true under `-race`. It now asserts
the shape of the outage and the recovery and tolerates a slow start, which is what the test
was ever about. Hammered 15 times under `-race` to be sure.

## How the new script handles what it is given

- **A verified dump before anything is touched.** `init` runs `backup.sh --verify`, which
  restores the dump into a scratch database and counts what came back, and refuses to go on
  if that fails. Adopting a live database is the most dangerous thing in this repository.
- The database and replication passwords go into `/etc/patroni/config.yml`, written **0600
  and owned by postgres**, which is the user Debian's `patroni.service` runs as.
- The join bundle holds both passwords, is written 0600, and is shut back to 0600 on arrival
  because `scp` does not preserve the mode. The script says it must be deleted from both
  machines and that it does not expire.
- No password is passed on a command line: the role is created by piping the statement into
  `psql`, so it is not in `/proc` for other users to read.
- Nothing new listens on the network from ForgeSync's side. etcd does, on 2379 and 2380, and
  the README says these machines should be able to reach each other and little else.

## Findings

**None in ForgeSync's own code.** `gosec` and `semgrep` were run in the previous gate against
this tree's Go, which this change does not touch beyond one test file.

**Seven faults in the new script, all found by running it**, none by reading: Debian's
`patroni.service` has a `ConditionPathExists` on its own config path; the DCS driver is
`python3-etcd` and without it Patroni exits offering only consul and kubernetes; Patroni
wants `postgresql.conf` inside the data directory and Debian keeps it in `/etc`; `member add`
creates a **voting** member, so adding one to a cluster of one makes the majority two and a
half-finished join wedges the database; a learner cannot answer `endpoint health` because
that commits a proposal; a member whose data was wiped but which the cluster still remembers
panics with `tocommit is out of range` unless it is removed and re-added; and `member
promote` fails on something that is already a voter. Each is fixed and each has a comment
saying why, because every one of them is the sort of thing that looks like a typo later.

## What this still does not do

ForgeSync does not watch the database. Patroni does, and `forgesync_database_up` says when a
controller cannot reach it, but nothing here alerts on replication lag or on a machine that
has been out of the cluster for a week. That belongs to whatever watches your PostgreSQL, and
`docs/LIMITATIONS.md` says so.

---

# Gate: three more package registries

Run on 2026-09-21 against the working tree on top of commit `4c9790e`, before committing.

## Classification

| | |
|---|---|
| **Scope** | The uncommitted diff |
| **Change under review** | `registryFetch` and `registryPublish` in `internal/replication/packages.go`, which carry nuget, rubygems and helm packages as well as generic and maven; `nodeBuiltFile`, which leaves out the files a node makes for itself; `plainSegment`, which refuses a piece of a package's identity that could climb out of a URL path; the test harness, which now speaks each registry's real upload protocol; the documentation that names what travels |
| **Exposure** | **Outbound HTTP to the Forgejo nodes**, carrying the service account's credentials. No new inbound surface: not one route, handler or request parser is added to the controller |
| **Data** | Package files, and the node tokens already used for every other registry call |
| **Privilege** | The service account, which is a site admin on its node |
| **Level** | **LEVEL 3, exposed code.** A URL path is built out of names chosen by whoever published the package, and the request carries a site-admin credential. |

## Result

| Check | Tool | Result |
|---|---|---|
| Formatting | `gofmt -l .` | **PASS** |
| Static checks | `go vet ./...` | **PASS** |
| Lint | `golangci-lint run` | **PASS**, 0 issues |
| Types (UI) | `tsc --noEmit` | **PASS** |
| Tests | `go test -count=1 -p 1 ./...` | **PASS**, 15 packages |
| Database tests | same run, `FORGESYNC_TEST_DATABASE_URL` at `forgesync_unit` | **PASS** |
| Race detection | `CGO_ENABLED=1 go test -race -p 1 ./...` | **PASS**, 15 packages. **The gap of the last four reviews is closed**: this host has a C compiler now and `make check` runs it |
| UI tests | `vitest run` | **PASS**, 80 of 80 in 14 files |
| New behaviour | `go test ./internal/replication` | **PASS**: each of the three types travels between three nodes, a settled run writes nothing, a single-file version is deleted whole, and the `.nuspec` is never touched |
| Negative case, planted | `nodeBuiltFile` made to return false for `.nuspec` | **FAIL as designed**: two `package_incomplete` conflicts, `400 not a nupkg: zip: not a valid zip file`. Exactly the fault the exclusion prevents, which is what says the test bites |
| Behaviour, end to end | live Forgejo 16 nodes SE and DK | **PASS**: a real `.nupkg`, `.gem` and chart published, fetched and republished by the paths ForgeSync uses, digests equal on both nodes; a version deleted whole |
| SAST | `gosec -quiet -exclude-dir=web ./...` | **PASS with findings**: 18, **the same 18 as the baseline**, measured by running it again against the stashed tree. None in the new code |
| SAST, second opinion | `semgrep p/golang p/security-audit p/secrets` | **PASS**: the same 3 as the baseline, none in the new code |
| Dependency vulnerabilities | `govulncheck ./...` | **PASS**, none; `go.mod` unchanged, no dependency added |
| Secrets, working tree | `gitleaks dir .` | **PASS**, no leaks |
| Secrets, history | `gitleaks git .` | **PASS**, 79 commits, no leaks |
| Shell | `shellcheck -S warning` on every tracked `*.sh` | **PASS** |
| Authorisation | `internal/api/security_test.go` | **NOT APPLICABLE**: no endpoint is added or changed. The walk still runs and still passes |
| DAST | `zap-baseline.py` | **NOT APPLICABLE**: the controller's HTTP surface is untouched by this diff. Last run in the previous gate, 0 failures |

### Tool versions and commands

```text
go            1.27.1    go vet ./... ; go test -count=1 -p 1 ./... ; CGO_ENABLED=1 go test -race -p 1 ./...
golangci-lint v2.13.2   golangci-lint run
gosec         v2.29.0   gosec -quiet -exclude-dir=web ./...
semgrep       1.177.0   semgrep scan --config=p/golang --config=p/security-audit --config=p/secrets
govulncheck   v1.8.0    govulncheck ./...
gitleaks      v8.30.1   gitleaks dir . ; gitleaks git .
shellcheck    0.10.0    shellcheck -S warning $(git ls-files '*.sh')
```

## What was fixed while reviewing it

**A package's path was laid out from names ForgeSync did not check** (LOW, hardening,
CWE-22). maven's path is built by hand, not escaped into one segment:
`org.scene:demo` becomes `org/scene/demo`. Nothing checked what those pieces were, so a
package whose group, artifact, version or file name held `..` or a slash would have been
fetched from, or published to, somewhere other than the package it meant. The same held for
the file name in every other type, because `url.PathEscape` leaves `..` alone: it is a
legal path segment, just not a legal name.

*Exploitable today?* **No**, and it was checked rather than assumed: Forgejo v16 refuses
`..` and `x/y` as a generic package name (404) and refuses a maven artifact of that shape
(400), so no node would report one. That is a validation on the far side of a network call,
which is the wrong place to depend on. `plainSegment` now refuses an empty piece, `.`, `..`,
and anything holding `/`, `\` or `%`, in both directions. A package that trips it is
reported like any other ForgeSync cannot carry, and nothing is read or written for it.
`TestPathPiecesThatCouldClimbOutAreRefused` covers thirteen shapes and checks that ordinary
ones still work, including a dotted maven group and a `-SNAPSHOT` version.

## Why these three types and not the others

The rule is not that the upload is simple. It is that **putting one file back recreates the
whole package**. nuget, rubygems and helm each store exactly one file per version, which was
read out of the Forgejo v16 source and then confirmed against a running node. So their
upload endpoint, which names nothing and works out for itself what it was given, is safe to
send that one file to. npm, pypi, composer, debian, rpm and the container registry each wrap
the file in something of their own or need metadata the package API never reports; for those
a `package_unreplicated` conflict naming the type stays the honest answer.

The one subtlety is a file nobody uploaded. Forgejo extracts a `.nuspec` from every `.nupkg`
and builds `maven-metadata.xml` out of what it holds, so both appear on a node that was
never sent one. Treating them as members would have made ForgeSync publish a fragment to an
endpoint that takes whole packages, which is what the planted failure above shows. They are
left out instead, and publishing the package recreates them: checked on a live node, the
`.nuspec` on the second node has the same sha256 as the first.

## What this still does not do

Nothing here makes a package type travel that could not before, beyond those three. The
count of types ForgeSync cannot carry is smaller; the reason for the rest is unchanged, and
`docs/LIMITATIONS.md` names them.

---

# Gate: two faults the running installation showed

Run on 2026-09-21 against the working tree on top of commit `7c69c83`, before committing.

## Classification

| | |
|---|---|
| **Scope** | The uncommitted diff |
| **Change under review** | Five places in `internal/issues/sync.go` that read a node's snapshot from a record's copies; the rule that the merge base only moves when every copy took part; `internal/nodes.fromRecord`, shared by `Resolve` and the node watcher so the two agree |
| **How they were found** | Not by reading. By looking at the running test installation before measuring anything on it, and seeing three controllers with uptimes of a few seconds |
| **Exposure** | Neither is reachable from a request. Both happen in the controller's own loops, on data the Forgejo nodes and the database supply |
| **Data** | None new. The node fingerprint hashes a token, as it already did |
| **Privilege** | The controller's own |
| **Level** | **LEVEL 3.** One is a remotely triggerable crash of the whole process (CWE-476), reached by a node going unreachable, and the other stops a controller doing any work at all. Availability, not confidentiality. |

## Result

| Check | Tool | Result |
|---|---|---|
| Formatting | `gofmt -l .` | **PASS** |
| Static checks | `go vet ./...` | **PASS** |
| Lint | `golangci-lint run` | **PASS**, 0 issues |
| Types (UI) | `tsc --noEmit` | **PASS** |
| Tests, with the database and the race detector | `CGO_ENABLED=1 go test -race -count=1 -p 1 ./...` | **PASS**, 15 packages |
| UI tests | `vitest run` | **PASS**, 80 of 80 in 14 files |
| New behaviour | `go test ./internal/issues ./internal/nodes` | **PASS**: a copy on a node that was not read is passed over, for both reasons a node can be missing, and the reaction still reaches it when it comes back; a config-file node is not read as a constant change, while a replaced token still is |
| Negative case, planted, crash | the skip removed at the reactions site | **FAIL as designed**: `panic: runtime error: invalid memory address or nil pointer dereference [signal SIGSEGV ... addr=0x28]`, the same signature as the live crash |
| Negative case, planted, restart loop | `fingerprintStored` put back as it was | **FAIL as designed**: the test prints the two fingerprints that could never match |
| Behaviour, end to end | the live test installation, rebuilt | **PASS**: three controllers, **0 restarts** against 2333 before, 0 panics over two complete rounds, and no "the installation's nodes have changed" at all. The condition that crashed it is still there: five issue copies are recorded on `us`, which is unreachable |
| SAST | `gosec -quiet -exclude-dir=web ./...` | **PASS with findings**: 18, the same 18 as the baseline |
| SAST, second opinion | `semgrep p/golang p/security-audit p/secrets` | **PASS**: the same 3 as the baseline |
| Dependency vulnerabilities | `govulncheck ./...` | **PASS**, none; `go.mod` unchanged |
| Secrets, working tree and history | `gitleaks dir .`, `gitleaks git .` | **PASS**, no leaks |
| Shell | `shellcheck -S warning` | **PASS**, nothing changed |
| Authorisation, DAST | | **NOT APPLICABLE**: no endpoint, route or handler is touched |

Tool versions and commands are as recorded in the previous gate; the same binaries were used.

## Findings

**HIGH, fixed: the controller crashed on any node it could not read.** A record's copies come
from ForgeSync's database and name every node an issue is on. What a run actually read is a
different set: a node that is unreachable, or that has issues turned off for that repository,
is passed over. Five per-member passes walked the copies and read the snapshot of whichever
node they named, which for such a node was nothing at all, so the process died on a nil
pointer. Any node going away took the whole controller down with it, every round, and no
round finished. Reachable by anyone who can stop a Forgejo node, or turn its issues off.

The fix skips a copy whose node was not read, and, separately, **holds the merge base still
unless every copy took part**. That second half matters as much: a base settled without a
node means that when the node comes back, whatever it has not got reads as something a person
deleted, and gets taken off every other node. The test covers exactly that, and it failed
before the second half was written.

A sixth path was found while fixing the five: issues turned off on the **primary** skipped it
before the check that a primary must be readable, leaving the run to read the primary's
snapshot as though it were there. The run now stops, which is the right answer since there is
nothing to copy from.

**MEDIUM, fixed: a controller restarted every fifteen seconds.** The node watcher restarts a
controller when the installation's nodes change, and compares a fingerprint to decide. It
built that fingerprint differently from the code that builds the node set: a node taken from
the config file keeps its token in a file, `Resolve` read it and the watcher left it empty.
The two could never agree, so the answer was always "changed". Every installation that keeps
its nodes in the config file was affected, which is every installation that predates the
admin UI. Both now go through one function, which is the only durable fix for two pieces of
code that have to agree.

## What this says about the reviews before it

Both faults were in code that passed a gate. Neither is subtle once seen, and neither was
going to be found by a scanner or a unit test written by the person who wrote the code: one
needed a node to go away, the other needed a config-file node and fifteen seconds. What found
them was looking at the thing running. So every gate from here reports what a running
installation did for a few minutes, as this one does: uptime, restart count, whether a round
finished. It is the cheapest check here and the only one that found either of these.

---

# Gate: a base settled without a node is a deletion nobody asked for

Run on 2026-09-21 against the working tree on top of commit `ba58bf7`, before committing.

## Classification

| | |
|---|---|
| **Scope** | The uncommitted diff |
| **Change under review** | `internal/replication/settle.go`, one function and the reason for it, and the seven merges that now go through it: packages, collaborators, branch protection, releases and their files, topics, Actions variables, an organization's teams and members |
| **How it was found** | By asking, after fixing the same thing in `internal/issues`, whether the rest of ForgeSync merged sets the same way. It did |
| **Exposure** | Not reachable from a request. It happens in the controller's own loops when a node is unhealthy or its read fails |
| **Data** | Published package files, people's repository access, branch protection rules, release files, topics, Actions variables, team membership |
| **Privilege** | The service account, which is a site admin on every node |
| **Level** | **LEVEL 3.** It destroys data on every node, and the trigger is one node being unreachable, which is ordinary. |

## Result

| Check | Tool | Result |
|---|---|---|
| Formatting | `gofmt -l .` | **PASS** |
| Static checks | `go vet ./...` | **PASS** |
| Lint | `golangci-lint run` | **PASS**, 0 issues |
| Types (UI) | `tsc --noEmit` | **PASS** |
| Tests, with the database and the race detector | `CGO_ENABLED=1 go test -race -count=1 -p 1 ./...` | **PASS**, 15 packages |
| New behaviour | `go test ./internal/replication` | **PASS**: a package published while a node was down survives that node coming back, and so does access granted meanwhile |
| Negative case, planted | `settle` made to ignore the count | **FAIL as designed**, and not only on the base: `se hasn't got the second version`, `dk hasn't got the second version`, `de hasn't got the second version`. The file was deleted from all three nodes because one of them was down when it was published |
| SAST | `gosec -quiet -exclude-dir=web ./...` | **PASS with findings**: 18, the same 18 as the baseline |
| SAST, second opinion | `semgrep p/golang p/security-audit p/secrets` | **PASS**: the same 3 as the baseline |
| Dependency vulnerabilities | `govulncheck ./...` | **PASS**, none; `go.mod` unchanged |
| Secrets, working tree and history | `gitleaks dir .`, `gitleaks git .` | **PASS**, no leaks |
| Running installation | the five-node test environment | **PASS**: 0 restarts, 0 panics, rounds finishing, while 1000 repositories replicate to four replicas |
| Authorisation, DAST | | **NOT APPLICABLE**: no endpoint, route or handler is touched |

## Findings

**HIGH, fixed: a node being unreachable could delete data on every other node.** Seven
merges record what the nodes last agreed on, and that record is what says which way a
member moved: one that appears where the record hasn't got it was added, one missing where
the record has it was taken away. The record was being written from whichever nodes the run
managed to read. A node that was down therefore had its silence read as agreement, and when
it came back, everything it had not got looked like a deletion and was carried out on every
other node.

The planted run says what that costs in the most concrete case: a package published on two
nodes while a third was down was **deleted from all three** on the next run. The other six
are the same shape, with access, protection rules, release files, topics, variables and team
membership in place of the file.

All seven now go through one function, which returns the old record unless every node that
has the thing was read. Holding it still costs nothing: the same comparison is made again
next run against the same record and writes nothing that was already written. The one thing
that waits with it is a real deletion, which reaches the other nodes once the node that was
away can be read. That is the same trade as never force-pushing over divergent history, and
`docs/LIMITATIONS.md` now says so under a heading of its own.

A node that answered "this feature is turned off here" is deliberately **not** counted. It
has nothing to say and never will, and counting it would freeze the record for good on any
installation with a fork in it.

---

# Gate: two things the tidying-up found

Run on 2026-09-21 against the working tree on top of commit `c3363b9`, before committing.

## Classification

| | |
|---|---|
| **Scope** | The uncommitted diff |
| **Change under review** | `Git.Forget`, and the call to it when ForgeSync forgets a repository; `deploy/test/scale.sh drop`, rewritten to page and to count what actually went; the documentation of what `replication.protect_replicas` means for a person pushing |
| **How they were found** | By clearing up after the scale run rather than by reading. `drop` reported success having deleted 50 of 1000, and the git cache held 1251 bare mirrors for 3 live repositories |
| **Exposure** | Neither is reachable from a request. One is a controller-side disk leak, the other is a test-environment script |
| **Data** | None. Both are about removing things ForgeSync itself created |
| **Privilege** | The controller's own, and the service account for the script |
| **Level** | **LEVEL 2.** Availability over a long horizon (the disk leak) and a test script that reported a job it had not done. |

## Result

| Check | Tool | Result |
|---|---|---|
| Formatting | `gofmt -l .` | **PASS** |
| Static checks | `go vet ./...` | **PASS** |
| Lint | `golangci-lint run` | **PASS**, 0 issues |
| Types (UI) | `tsc --noEmit` | **PASS** |
| Tests, with the database and the race detector | `CGO_ENABLED=1 go test -race -count=1 -p 1 ./...` | **PASS**, 15 packages |
| New behaviour | `go test ./internal/replication` | **PASS**: `Forget` takes a repository's cache and its wiki's, leaves another repository's alone, is not an error twice, and refuses a name that is not an id; the deletion test now asserts the cache is gone once the repository is forgotten |
| Negative case, planted | the `Forget` call removed | **FAIL as designed**: `11111111-...-111111111111.git is still there after the repository was forgotten` |
| Behaviour, end to end | five live Forgejo nodes | **PASS**: 1000 repositories deleted on their primary, 3990 archived copies then removed across four nodes in 55s, every node back to the 3 repositories it started with |
| SAST | `gosec -quiet -exclude-dir=web ./...` | **PASS with findings**: 18, the same 18 as the baseline |
| SAST, second opinion | `semgrep p/golang p/security-audit p/secrets` | **PASS**: the same 3 as the baseline |
| Dependency vulnerabilities | `govulncheck ./...` | **PASS**, none |
| Secrets, working tree and history | `gitleaks dir .`, `gitleaks git .` | **PASS**, no leaks |
| Shell | `shellcheck -S warning` on every tracked `*.sh` | **PASS**, including the rewritten `scale.sh` |
| Authorisation, DAST | | **NOT APPLICABLE**: no endpoint, route or handler is touched |

## Findings

**MEDIUM, fixed: a forgotten repository kept its whole history on the controller for ever.**
When the backup period is over and no copy is left anywhere, ForgeSync forgets a repository:
the row goes, the archives go. The bare mirror under `replication.work_dir` did not, and
nothing anywhere removed one. On an installation where repositories come and go that grows
without bound, and the git cache is already the largest thing a controller keeps, which
`deploy/prod/README.md` tells people to watch. `Git.Forget` now removes the repository's
cache and its wiki's when the repository is forgotten. A failure there is logged and no
more: tidying must not be able to stop the forgetting it belongs to.

**LOW, fixed: `scale.sh drop` said it had cleared a node it had barely touched.** It asked
for `limit=200` and took one page. Forgejo caps a search response at `MAX_RESPONSE_ITEMS`,
50 by default, so it deleted 50 of 1000 and printed "cleared". Two further faults came out
of fixing it, both mine, both worth recording because they are the same mistake in different
clothes: counting attempts rather than successes made the loop ask for the same page for
ever when the deletes were refused, and the refusals happened because the helper it used
sudoes as the repository's owner, who cannot see the private archive organization at all and
gets 404 for everything in it. It now counts only what actually went, stops when a page
yields nothing, and uses a non-sudoed call for the archives.

**Documented, not a fault: what `protect_replicas` means for a person.** With it on, which
is what the production configuration sets, a user's push to any node that is not the
repository's primary is refused by Forgejo. The rejection is Forgejo's own protected-branch
message: it does not mention ForgeSync and does not name the node they should have used, and
it cannot be made to without patching Forgejo, which this project will not do. That is a
real rough edge, so it is now written down in `docs/LIMITATIONS.md` with what to do instead,
rather than left for somebody to discover.
