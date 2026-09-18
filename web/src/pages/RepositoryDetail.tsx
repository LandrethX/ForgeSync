import { useEffect, useState } from "react";
import { api, ApiError, hasRole, type Repository } from "../api";
import { ErrorNote, PageHeader } from "../components/Layout";
import { PresenceLabel, REPO_STATUS, RepoStatusBadge } from "../components/StatusBadge";
import { formatAgo, formatDateTime } from "../format";
import { useLoad, useNodes, useNow, useSession } from "../hooks";
import { Link } from "../router";

export function RepositoryDetail({ id }: { id: string }) {
  const repo = useLoad(() => api.repository(id), [id]);
  const { nodes } = useNodes();
  const now = useNow(5000);

  if (repo.error) {
    return (
      <>
        <p className="breadcrumb">
          <Link to="/repositories">Repositories</Link>
        </p>
        <ErrorNote message={repo.error} />
      </>
    );
  }
  const r = repo.data;
  const nodeURL = (name: string) => nodes?.find((n) => n.name === name)?.url;

  return (
    <>
      <p className="breadcrumb">
        <Link to="/repositories">Repositories</Link> / {r?.full_name ?? "…"}
      </p>
      <PageHeader title={r?.full_name ?? "Repository"}>{r && <RepoStatusBadge status={r.status} />}</PageHeader>
      {r && <p className="muted page-intro">{REPO_STATUS[r.status].description}</p>}

      {r && (
        <>
          <PrimaryPanel repo={r} onSaved={repo.reload} />

          <h2>On each node</h2>
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th scope="col">Node</th>
                  <th scope="col">Present</th>
                  <th scope="col">Default branch</th>
                  <th scope="col">Head commit</th>
                  <th scope="col">Visibility</th>
                  <th scope="col">Flags</th>
                  <th scope="col">Updated in Forgejo</th>
                  <th scope="col">Last found</th>
                </tr>
              </thead>
              <tbody>
                {r.nodes.map((v) => {
                  const rp = v.replica;
                  const url = nodeURL(v.node);
                  const flags = rp
                    ? [rp.mirror && "Mirror", rp.fork && "Fork", rp.archived && "Archived", rp.empty && "Empty"].filter(Boolean)
                    : [];
                  return (
                    <tr key={v.node}>
                      <th scope="row">
                        {v.node}
                        {v.stale && <div className="muted small">latest scan failed; older data</div>}
                      </th>
                      <td>
                        <PresenceLabel presence={v.presence} />
                        {v.presence === "present" && url && (
                          <div className="small">
                            <a href={`${url}/${r.full_name}`} target="_blank" rel="noreferrer noopener">
                              Open in Forgejo
                            </a>
                          </div>
                        )}
                      </td>
                      <td>{rp?.present ? rp.default_branch || "–" : "–"}</td>
                      <td className="mono">
                        {rp?.present ? (rp.head_error ? <span title={rp.head_error}>unreadable</span> : rp.head_sha.slice(0, 12) || "–") : "–"}
                      </td>
                      <td>{rp?.present ? (rp.private ? "Private" : "Public") : "–"}</td>
                      <td>{flags.length ? flags.join(", ") : "–"}</td>
                      <td className="num" title={rp?.forgejo_updated_at ? formatDateTime(rp.forgejo_updated_at) : undefined}>
                        {rp?.present && rp.forgejo_updated_at ? formatAgo(rp.forgejo_updated_at, now) : "–"}
                      </td>
                      <td className="num" title={rp?.last_seen_at ? formatDateTime(rp.last_seen_at) : undefined}>
                        {rp?.last_seen_at ? formatAgo(rp.last_seen_at, now) : "–"}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
          <p className="muted small">
            ForgeSync ID <span className="mono">{r.id}</span>, first found {formatDateTime(r.first_seen_at)}.
          </p>
        </>
      )}
    </>
  );
}

function PrimaryPanel({ repo, onSaved }: { repo: Repository; onSaved: () => void }) {
  const session = useSession();
  const canEdit = hasRole(session, "administrator");
  const [choice, setChoice] = useState(repo.primary_node);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<{ ok: boolean; text: string }>();

  useEffect(() => setChoice(repo.primary_node), [repo.primary_node]);

  async function save() {
    setBusy(true);
    setMessage(undefined);
    try {
      await api.setPrimary(repo.id, choice);
      setMessage({ ok: true, text: choice ? `Primary set to ${choice}.` : "Primary cleared." });
      onSaved();
    } catch (e) {
      setMessage({ ok: false, text: e instanceof ApiError ? e.message : "Saving failed." });
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="panel" aria-labelledby="primary-heading">
      <h2 id="primary-heading">Primary node</h2>
      <p className="muted">
        The node that's authoritative for this repository. For now this is only recorded: ForgeSync doesn't replicate
        yet, so it has no effect.
      </p>
      {canEdit ? (
        <div className="toolbar">
          <label htmlFor="primary">Primary</label>
          <select id="primary" value={choice} onChange={(e) => setChoice(e.target.value)}>
            <option value="">Not set</option>
            {repo.nodes.map((v) => (
              <option key={v.node} value={v.node}>
                {v.node}
                {v.presence !== "present" ? " (doesn't have it)" : ""}
              </option>
            ))}
          </select>
          <button type="button" className="button-primary" onClick={save} disabled={busy || choice === repo.primary_node}>
            {busy ? "Saving…" : "Save"}
          </button>
          {message && (
            <span role="status" className={message.ok ? "muted" : "error-inline"}>
              {message.text}
            </span>
          )}
        </div>
      ) : (
        <p>
          <strong>{repo.primary_node || "Not set"}</strong>
          <span className="muted"> · Only administrators can change it.</span>
        </p>
      )}
    </section>
  );
}
