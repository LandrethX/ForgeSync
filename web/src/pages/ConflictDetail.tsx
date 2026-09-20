import { useEffect, useState, type FormEvent } from "react";
import { api, ApiError, hasRole, type Conflict } from "../api";
import { ErrorNote, PageHeader } from "../components/Layout";
import { StatusIcon, type Tone } from "../components/StatusBadge";
import {
  conflictExplanation,
  conflictFix,
  conflictSides,
  conflictTitle,
  relationText,
} from "../conflictText";
import { formatDateTime } from "../format";
import { useLoad, useNodes, useSession } from "../hooks";
import { Link } from "../router";

/** How a conflict's state reads, and how loudly. */
const STATE_LABEL: Record<string, string> = {
  open: "Open",
  dismissed: "Dismissed",
  cleared: "Cleared",
};

const STATE_TONE: Record<string, Tone> = {
  open: "serious",
  dismissed: "neutral",
  cleared: "good",
};

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
          <StatusIcon tone={STATE_TONE[c.state] ?? "good"} />
          <span>{STATE_LABEL[c.state] ?? c.state}</span>
        </span>
      </PageHeader>
      <p className="page-intro">
        In <Link to={`/repositories/${c.repository_id}`}>{c.full_name}</Link>
      </p>

      <section className="panel" aria-labelledby="what-heading">
        <h2 id="what-heading">What's different</h2>
        {/* A deletion conflict has no per-node value: the sentence says it all. */}
        {conflictSides(c).length > 0 && (
          <div className="table-wrap flat">
            <table>
              <thead>
                <tr>
                  <th scope="col">Node</th>
                  <th scope="col">
                    {c.kind === "default_branch_mismatch"
                      ? "Default branch"
                      : c.kind === "issue_conflict"
                        ? "Value"
                        : "Commit"}
                  </th>
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
                        {c.kind !== "default_branch_mismatch" &&
                        c.kind !== "issue_conflict" &&
                        url &&
                        v ? (
                          <a
                            href={`${url}/${c.full_name}/commit/${v}`}
                            target="_blank"
                            rel="noreferrer noopener"
                          >
                            {v.slice(0, 12)}
                          </a>
                        ) : (
                          v || <span className="muted">not there</span>
                        )}
                      </td>
                      <td>
                        {node === primary && (
                          <span className="tag">Primary</span>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
        {conflictExplanation(c) && <p>{conflictExplanation(c)}</p>}
        {c.details.relations && c.details.relations.length > 0 && (
          <ul className="relations">
            {c.details.relations.map((r) => (
              <li key={`${r.a}-${r.b}`}>{relationText(r)}</li>
            ))}
          </ul>
        )}
        {!primary && (
          <p className="muted">
            No primary is set for this repository yet. It's set automatically to
            the node where the repository was created first, after the next
            complete scan; an administrator can{" "}
            <Link to={`/repositories/${c.repository_id}`}>choose another</Link>.
          </p>
        )}
      </section>

      {c.state === "open" &&
        c.details.handoffs &&
        c.details.handoffs.length > 0 && (
          <section className="panel" aria-labelledby="owner-heading">
            <h2 id="owner-heading">Waiting for the owner</h2>
            <p>
              ForgeSync handed this to the repository's owner as a pull request
              on {primary}. Merging it keeps the other sites' commits; closing
              it without merging keeps {primary}'s version, and ForgeSync then
              resets those sites (their commits stay on the pull request's
              branch for a while as a backup).
            </p>
            <ul>
              {c.details.handoffs.map((h) => (
                <li key={h.pr_number}>
                  <a href={h.pr_url} target="_blank" rel="noreferrer noopener">
                    Pull request #{h.pr_number}
                  </a>{" "}
                  for {h.nodes.join(", ")} (branch{" "}
                  <span className="mono">{h.branch}</span>)
                </li>
              ))}
            </ul>
          </section>
        )}

      {c.state === "open" &&
        !(c.details.handoffs && c.details.handoffs.length > 0) && (
          <section className="panel" aria-labelledby="fix-heading">
            <h2 id="fix-heading">How to fix it</h2>
            <p>{conflictFix(c)}</p>
            <p className="muted">
              The next check clears this conflict once the nodes agree.
            </p>
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
          {c.dismissed_at && (
            <>
              <dt>Dismissed</dt>
              <dd>
                {formatDateTime(c.dismissed_at)} by {c.dismissed_by}
              </dd>
            </>
          )}
        </dl>
      </section>
    </>
  );
}

function Acknowledgement({
  conflict,
  onSaved,
}: {
  conflict: Conflict;
  onSaved: () => void;
}) {
  const session = useSession();
  const [note, setNote] = useState(conflict.note ?? "");
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<{ ok: boolean; text: string }>();
  useEffect(() => setNote(conflict.note ?? ""), [conflict.note]);

  const canEdit = hasRole(session, "operator");
  if (!canEdit && !conflict.acknowledged_by && !conflict.dismissed_by)
    return null;

  async function run(what: () => Promise<unknown>, ok: string) {
    setBusy(true);
    setMessage(undefined);
    try {
      await what();
      setMessage({ ok: true, text: ok });
      onSaved();
    } catch (err) {
      setMessage({
        ok: false,
        text: err instanceof ApiError ? err.message : "That didn't work.",
      });
    } finally {
      setBusy(false);
    }
  }

  async function submit(e: FormEvent) {
    e.preventDefault();
    await run(() => api.acknowledgeConflict(conflict.id, note), "Saved.");
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
              {busy
                ? "Saving…"
                : conflict.acknowledged_by
                  ? "Update acknowledgement"
                  : "Acknowledge"}
            </button>
            {message && (
              <span
                role="status"
                className={message.ok ? "muted" : "error-inline"}
              >
                {message.text}
              </span>
            )}
          </div>
          <p className="muted small">
            This is only a record for the team; it doesn't change anything on
            the nodes.
          </p>
        </form>
      )}
      {canEdit && conflict.state === "open" && (
        <div className="dismiss">
          <h3>Not one for ForgeSync?</h3>
          <p className="muted small">
            Some differences aren&rsquo;t ForgeSync&rsquo;s to settle: a
            secret it can&rsquo;t copy, a node you&rsquo;ve decided to leave as
            it is. Dismissing keeps the conflict and the note, and stops it
            being counted. It comes back if what it says changes, or if it
            clears and happens again.
          </p>
          <button
            type="button"
            className="button-quiet"
            disabled={busy}
            onClick={() =>
              run(
                () => api.dismissConflict(conflict.id, note),
                "Dismissed; it isn't counted any more.",
              )
            }
          >
            Dismiss this conflict
          </button>
        </div>
      )}
      {conflict.state === "dismissed" && (
        <div className="dismiss">
          <p>
            <strong>{conflict.dismissed_by}</strong> dismissed this
            {conflict.dismissed_at
              ? ` on ${formatDateTime(conflict.dismissed_at)}`
              : ""}
            . It isn&rsquo;t counted; ForgeSync still checks it, and brings it
            back if what it says changes.
          </p>
          {canEdit && (
            <button
              type="button"
              className="button-quiet"
              disabled={busy}
              onClick={() =>
                run(() => api.reopenConflict(conflict.id), "It's open again.")
              }
            >
              Bring it back
            </button>
          )}
        </div>
      )}
    </section>
  );
}
