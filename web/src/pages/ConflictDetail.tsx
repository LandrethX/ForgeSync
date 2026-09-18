import { useEffect, useState, type FormEvent } from "react";
import { api, ApiError, hasRole, type Conflict } from "../api";
import { ErrorNote, PageHeader } from "../components/Layout";
import { StatusIcon } from "../components/StatusBadge";
import { conflictSides, conflictTitle, relationText } from "../conflictText";
import { formatDateTime } from "../format";
import { useLoad, useNodes, useSession } from "../hooks";
import { Link } from "../router";

export function ConflictDetail({ id }: { id: number }) {
  const conflict = useLoad(() => api.conflict(id), [id]);
  const { nodes } = useNodes();
  const c = conflict.data;

  if (conflict.error) {
    return (
      <>
        <p className="breadcrumb">
          <Link to="/conflicts">Conflicts</Link>
        </p>
        <ErrorNote message={conflict.error} />
      </>
    );
  }
  if (!c) return <p className="muted">Loading…</p>;

  const nodeURL = (name: string) => nodes?.find((n) => n.name === name)?.url;
  const primary = c.primary_node;

  return (
    <>
      <p className="breadcrumb">
        <Link to="/conflicts">Conflicts</Link> / #{c.id}
      </p>
      <PageHeader title={conflictTitle(c)}>
        <span className="status-badge">
          <StatusIcon tone={c.state === "open" ? "serious" : "good"} />
          <span>{c.state === "open" ? "Open" : "Cleared"}</span>
        </span>
      </PageHeader>
      <p className="page-intro">
        In <Link to={`/repositories/${c.repository_id}`}>{c.full_name}</Link>
      </p>

      <section className="panel" aria-labelledby="what-heading">
        <h2 id="what-heading">What's different</h2>
        <div className="table-wrap flat">
          <table>
            <thead>
              <tr>
                <th scope="col">Node</th>
                <th scope="col">{c.kind === "git_diverged" ? `Head of ${c.details.branch ?? "the branch"}` : "Default branch"}</th>
                <th scope="col" />
              </tr>
            </thead>
            <tbody>
              {conflictSides(c).map(([node, v]) => {
                const url = nodeURL(node);
                return (
                  <tr key={node}>
                    <th scope="row">{node}</th>
                    <td className="mono">
                      {c.kind === "git_diverged" && url ? (
                        <a href={`${url}/${c.full_name}/commit/${v}`} target="_blank" rel="noreferrer noopener">
                          {v.slice(0, 12)}
                        </a>
                      ) : (
                        v
                      )}
                    </td>
                    <td>{node === primary && <span className="tag">Primary</span>}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
        {c.details.relations && c.details.relations.length > 0 && (
          <ul className="relations">
            {c.details.relations.map((r) => (
              <li key={`${r.a}-${r.b}`}>{relationText(r)}</li>
            ))}
          </ul>
        )}
        {!primary && (
          <p className="muted">
            No primary is set for this repository.{" "}
            <Link to={`/repositories/${c.repository_id}`}>Set one</Link> to record which side is authoritative.
          </p>
        )}
      </section>

      {c.state === "open" && (
        <section className="panel" aria-labelledby="fix-heading">
          <h2 id="fix-heading">How to fix it</h2>
          {c.kind === "git_diverged" ? (
            <p>
              ForgeSync never overwrites diverged history. Someone who knows the repository has to reconcile it: start
              from {primary ? `the primary (${primary})` : "the side you trust"}, merge or rebase the other node's commits
              into it, and push the result to every node. The next scan clears this conflict once the nodes agree, or
              the others are simply behind.
            </p>
          ) : (
            <p>
              Pick one default branch and set it in the repository settings on every node that differs. The next scan
              clears this conflict.
            </p>
          )}
        </section>
      )}

      <Acknowledgement conflict={c} onSaved={conflict.reload} />

      <section aria-labelledby="history-heading">
        <h2 id="history-heading">History</h2>
        <dl className="facts facts-wide">
          <dt>Detected</dt>
          <dd>{formatDateTime(c.detected_at)}</dd>
          <dt>{c.state === "open" ? "Last seen" : "Last seen open"}</dt>
          <dd>{formatDateTime(c.last_seen_at)}</dd>
          {c.cleared_at && (
            <>
              <dt>Cleared</dt>
              <dd>{formatDateTime(c.cleared_at)}</dd>
            </>
          )}
          {c.acknowledged_at && (
            <>
              <dt>Acknowledged</dt>
              <dd>
                {formatDateTime(c.acknowledged_at)} by {c.acknowledged_by}
              </dd>
            </>
          )}
        </dl>
      </section>
    </>
  );
}

function Acknowledgement({ conflict, onSaved }: { conflict: Conflict; onSaved: () => void }) {
  const session = useSession();
  const [note, setNote] = useState(conflict.note ?? "");
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<{ ok: boolean; text: string }>();
  useEffect(() => setNote(conflict.note ?? ""), [conflict.note]);

  const canEdit = hasRole(session, "operator");
  if (!canEdit && !conflict.acknowledged_by) return null;

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setMessage(undefined);
    try {
      await api.acknowledgeConflict(conflict.id, note);
      setMessage({ ok: true, text: "Saved." });
      onSaved();
    } catch (err) {
      setMessage({ ok: false, text: err instanceof ApiError ? err.message : "Saving failed." });
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="panel" aria-labelledby="ack-heading">
      <h2 id="ack-heading">Acknowledgement</h2>
      {conflict.acknowledged_by && (
        <p>
          <strong>{conflict.acknowledged_by}</strong> acknowledged this
          {conflict.note ? (
            <>
              : <q>{conflict.note}</q>
            </>
          ) : (
            "."
          )}
        </p>
      )}
      {canEdit && (
        <form onSubmit={submit} className="ack-form">
          <label htmlFor="ack-note">Note for others (optional)</label>
          <textarea
            id="ack-note"
            rows={3}
            maxLength={1000}
            value={note}
            onChange={(e) => setNote(e.target.value)}
            placeholder="Who is fixing it, and how"
          />
          <div className="toolbar">
            <button type="submit" className="button-primary" disabled={busy}>
              {busy ? "Saving…" : conflict.acknowledged_by ? "Update acknowledgement" : "Acknowledge"}
            </button>
            {message && (
              <span role="status" className={message.ok ? "muted" : "error-inline"}>
                {message.text}
              </span>
            )}
          </div>
          <p className="muted small">This is only a record for the team; it doesn't change anything on the nodes.</p>
        </form>
      )}
    </section>
  );
}
