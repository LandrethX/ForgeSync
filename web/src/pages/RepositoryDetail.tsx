import { useEffect, useState } from "react";
import { api, ApiError, hasRole, type ReplicaState, type Repository } from "../api";
import { ErrorNote, PageHeader } from "../components/Layout";
import { PresenceLabel, REPO_STATUS, RepoStatusBadge, StatusIcon, type Tone } from "../components/StatusBadge";
import { conflictTitle } from "../conflictText";
import { formatAgo, formatDateTime, formatDuration } from "../format";
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
          <RepoConflicts repositoryId={r.id} />
          <PrimaryPanel repo={r} onSaved={repo.reload} />
          <ReplicationPanel repo={r} onChange={repo.reload} />

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
        The node that's authoritative for this repository.{" "}
        {repo.replication?.enabled
          ? "Replication copies its branches and tags to the other nodes; changing it changes where they're copied from."
          : "While replication is off, it's only recorded."}
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

function RepoConflicts({ repositoryId }: { repositoryId: string }) {
  const open = useLoad(() => api.conflicts({ state: "open", repository: repositoryId, limit: 20, offset: 0 }), [repositoryId]);
  if (!open.data?.items?.length) return null;
  return (
    <section className="panel conflict-panel" aria-labelledby="repo-conflicts-heading">
      <h2 id="repo-conflicts-heading">
        <StatusIcon tone="serious" /> Open conflicts
      </h2>
      <ul>
        {open.data.items.map((c) => (
          <li key={c.id}>
            <Link to={`/conflicts/${c.id}`}>{conflictTitle(c)}</Link>
            <span className="muted"> · detected {formatDateTime(c.detected_at)}</span>
          </li>
        ))}
      </ul>
    </section>
  );
}

const REPLICA_STATE: Record<ReplicaState, { tone: Tone; label: string }> = {
  synced: { tone: "good", label: "In sync" },
  conflict: { tone: "serious", label: "Conflict" },
  error: { tone: "critical", label: "Error" },
  waiting: { tone: "warning", label: "Waiting" },
  missing: { tone: "serious", label: "Repository missing" },
};

function ReplicationPanel({ repo, onChange }: { repo: Repository; onChange: () => void }) {
  const session = useSession();
  const now = useNow(5000);
  const [message, setMessage] = useState<{ ok: boolean; text: string }>();
  const [polling, setPolling] = useState(false);
  const repl = repo.replication;

  // After "Replicate now", reload a few times so the result shows up.
  useEffect(() => {
    if (!polling) return;
    let n = 0;
    const id = window.setInterval(() => {
      onChange();
      if (++n >= 5) {
        window.clearInterval(id);
        setPolling(false);
      }
    }, 2000);
    return () => window.clearInterval(id);
  }, [polling, onChange]);

  async function replicate() {
    setMessage(undefined);
    try {
      const r = await api.replicateNow(repo.id);
      setMessage({ ok: true, text: r.queued ? "Replication started." : "Replication is already running." });
      setPolling(true);
    } catch (e) {
      setMessage({ ok: false, text: e instanceof ApiError ? e.message : "Couldn't start replication." });
    }
  }

  if (!repl?.enabled) {
    return (
      <section className="panel" aria-labelledby="repl-heading">
        <h2 id="repl-heading">Replication</h2>
        <p className="muted">Replication is turned off on this controller (replication.enabled in its config).</p>
      </section>
    );
  }
  return (
    <section className="panel" aria-labelledby="repl-heading">
      <div className="section-header">
        <h2 id="repl-heading">Replication</h2>
        {repo.primary_node && hasRole(session, "operator") && (
          <button type="button" className="button-quiet" onClick={replicate} disabled={polling}>
            {polling ? "Replicating…" : "Replicate now"}
          </button>
        )}
      </div>
      {!repo.primary_node ? (
        <p className="muted">Set a primary to start replicating this repository.</p>
      ) : repl.replicas.length === 0 ? (
        <p className="muted">
          Branches and tags are copied from {repo.primary_node} after each scan. It hasn't run for this repository yet.
        </p>
      ) : (
        <div className="table-wrap flat">
          <table>
            <thead>
              <tr>
                <th scope="col">Replica</th>
                <th scope="col">State</th>
                <th scope="col">Last in sync</th>
                <th scope="col">Out of sync for</th>
                <th scope="col">Last attempt</th>
              </tr>
            </thead>
            <tbody>
              {repl.replicas.map((x) => {
                const info = REPLICA_STATE[x.state] ?? { tone: "neutral" as Tone, label: x.state };
                return (
                  <tr key={x.node}>
                    <th scope="row">{x.node}</th>
                    <td>
                      <span className="status-badge">
                        <StatusIcon tone={info.tone} />
                        <span>{info.label}</span>
                      </span>
                      {x.detail && <div className="small replica-detail">{x.detail}</div>}
                      {x.state === "conflict" && (
                        <div className="small">
                          <Link to="/conflicts">See conflicts</Link>
                        </div>
                      )}
                    </td>
                    <td className="num" title={x.last_success_at ? formatDateTime(x.last_success_at) : undefined}>
                      {x.last_success_at ? formatAgo(x.last_success_at, now) : "Never"}
                    </td>
                    <td className="num">
                      {x.out_of_sync_since ? formatDuration(now - Date.parse(x.out_of_sync_since)) : "–"}
                    </td>
                    <td className="num" title={formatDateTime(x.last_attempt_at)}>
                      {formatAgo(x.last_attempt_at, now)}
                      {x.refs_updated > 0 && <div className="small muted">{x.refs_updated} ref(s) updated</div>}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      {message && (
        <p role="status" className={message.ok ? "muted" : "error-inline"}>
          {message.text}
        </p>
      )}
      <p className="muted small">
        Only fast-forwards, new refs and deletions of what ForgeSync wrote itself are copied. Anything else becomes a
        conflict. Missing repositories aren't created on replicas yet.
      </p>
    </section>
  );
}
