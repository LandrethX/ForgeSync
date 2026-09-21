# Security

## Reporting something

Open a private security advisory on the repository
(<https://github.com/LandrethX/ForgeSync/security/advisories/new>) rather than a public
issue. Say what you did, what happened, and what you expected; a proof of concept against
the `deploy/test` environment is the most useful thing you can attach, since that
environment is disposable and its credentials are throwaway by design.

## What ForgeSync holds

A controller is the most privileged thing in a Forgejo installation that runs it. It
holds a site-admin token for every node, it acts as other users through `Sudo`, and it can
create accounts and repositories. Treat the controller host as you would treat the Forgejo
hosts themselves.

| Secret | Where it lives | What it can do |
|---|---|---|
| Node tokens | A file named in the config, or sealed in the database with the node key | Everything on that Forgejo node |
| Node key | A file named by `node_key_file`, the same on every controller | Opens the node tokens in the database |
| Admin token | A file named in the config | The whole admin API, as Administrator |
| Webhook secret | A file named in the config | Per-node secrets are derived from it |
| Database URL | A file, or `FORGESYNC_DATABASE_URL` | All of ForgeSync's state |
| Account passwords | PBKDF2-HMAC-SHA256, 600,000 iterations, per-account salt | Sign in at the role the account holds |
| Session cookies | Only the SHA-256 of the value is stored | Act as that account until it expires |

**No user credential appears in that table, and that is not an oversight.** Everybody who
uses the nodes signs in with [SceneID](https://id.scene.org/) over OIDC, so a regular user
has no password on a Forgejo node and none on ForgeSync. What ForgeSync keeps about a person
is their SceneID subject, their login name, which nodes they have an account on and which
node is their primary. Every secret above is an administrator's credential for the
infrastructure, which a control plane must hold to act at all, and not anybody's personal
one.

No secret is ever written to a log or returned by the API. A token reaches git through an
`http.extraHeader` set in the environment, never in a command line and never on disk.

A node's token is written down only when the node is kept in the database, and then it is
sealed with AES-256-GCM under the node key, which the controllers hold and the database
never sees. That is what keeps a dump, a nightly backup, a streaming standby or a restore
into a scratch database from being a set of site-admin tokens for every node. Somebody who
has a controller already has those tokens, because it is using them; the key adds nothing
against them and is not meant to.

## How the interface is protected

- **Authentication.** A bearer token (the CLI, counting as Administrator) or a session
  cookie. Sessions live in the shared database, so signing in on one controller signs you
  in on every one of them and a failover does not sign anyone out; the cookie is HttpOnly,
  SameSite=Strict and Secure unless `http.secure_cookies` is turned off for local
  development. Absolute lifetime 8 hours, idle timeout 30 minutes, and the idle clock only
  moves on a real request.
- **Authorisation.** Viewer < Operator < Administrator, checked by `requireRole` on every
  route. `internal/api/security_test.go` walks every write endpoint against nobody signed
  in, each role below the one it needs, and the role itself. Adding or retiring a node is
  Administrator, and the token given with it is sealed before it is stored and never
  returned by anything: what is audited is that a node was added and by whom.
- **CSRF.** A cookie-authenticated write needs the `X-ForgeSync-CSRF` header, which another
  site cannot set without a preflight this server never allows.
- **Sign-in throttling.** Ten failures from one client address in five minutes, then 429.
  Behind a reverse proxy, set `http.trusted_proxies` to the proxy's address, or every
  client shares one and ten wrong passwords lock out the installation.
- **Webhook deliveries** are authenticated by an HMAC-SHA256 over the body, per node,
  checked before the body is parsed.
- **Headers.** A content security policy with no inline scripts or styles, `nosniff`,
  `DENY` framing, `no-referrer`, same-origin opener and resource policies, and HSTS on
  requests that arrived over TLS. Where a proxy terminates TLS, the proxy is the one that
  has to send HSTS.
- **Audit.** Every sign-in, refusal, sign-out and write is recorded with the actor, the
  action, the client address and the time, and the history cannot be edited through the
  API.

## What is deliberately not done

- **Deploy keys and personal access tokens are not replicated.** Copying them would spread
  credentials a person chose to hold on one node.
- **A repository is never made public.** Private travels one way: private anywhere becomes
  private everywhere.
- **Nothing is force-pushed on ForgeSync's own judgement**, and no conflicting metadata is
  discarded. A difference that cannot be settled without deciding for someone becomes a
  conflict for a person.
- **ForgeSync's own administrators get no account on any Forgejo node.** They administer
  the controllers, which is a different thing from using the nodes.

`docs/LIMITATIONS.md` has the full list, including what Forgejo will not let ForgeSync do
at all.

## Keeping it that way

`CONTRIBUTING.md` has the pre-release pass: golangci-lint, govulncheck, gosec, gitleaks,
osv-scanner, shellcheck and `npm audit`. `.golangci.yml` and `.gitleaks.toml` record what
was examined and deliberately kept, with the reason.
`docs/SECURITY_REVIEW.md` is the last full review, with what each tool found and what was
done about it.
