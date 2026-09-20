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

**INFORMATIONAL: the sign-in limiter is per controller, in memory.** Two controllers mean
twice the attempts before a block, and a failover clears the window. With a 12-character
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
