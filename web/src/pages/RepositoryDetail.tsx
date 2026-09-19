import { useEffect, useState } from "react";
import {
  api,
  ApiError,
  hasRole,
  type ReplicaState,
  type Repository,
} from "../api";
import { ErrorNote, PageHeader } from "../components/Layout";
import {
  PresenceLabel,
  REPO_STATUS,
  RepoStatusBadge,
  StatusIcon,
  type Tone,
} from "../components/StatusBadge";
import { conflictTitle } from "../conflictText";
import { formatAgo, formatDateTime, formatDuration } from "../format";
import { useLeadership, useLoad, useNodes, useNow, useSession } from "../hooks";
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
      <PageHeader title={r?.full_name ?? "Repository"}>
        {r && <RepoStatusBadge status={r.status} />}
      </PageHeader>
      {r && (
        <p className="muted page-intro">{REPO_STATUS[r.status].description}</p>
      )}

      {r && (
        <>
          {(r.deleted_at || (r.archives && r.archives.length > 0)) && (
            <ArchivePanel repo={r} />
          )}
          <RepoConflicts repositoryId={r.id} />
          <PrimaryPanel repo={r} onSaved={repo.reload} />
          <ReplicationPanel repo={r} onChange={repo.reload} />
          {r.replication?.enabled && (
            <IssuesPanel
              repositoryId={r.id}
              nodeNames={r.nodes.map((v) => v.node)}
            />
          )}

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
                    ? [
                        rp.mirror && "Mirror",
                        rp.fork && "Fork",
                        rp.archived && "Archived",
                        rp.empty && "Empty",
                      ].filter(Boolean)
                    : [];
                  return (
                    <tr key={v.node}>
                      <th scope="row">
                        {v.node}
                        {v.stale && (
                          <div className="muted small">
                            latest scan failed; older data
                          </div>
                        )}
                      </th>
                      <td>
                        <PresenceLabel presence={v.presence} />
                        {rp?.full_name &&
                          rp.full_name.toLowerCase() !==
                            r.full_name.toLowerCase() && (
                            <div className="small">
                              Still named {rp.full_name} here; renamed on the
                              next replication
                            </div>
                          )}
                        {v.presence === "present" && url && (
                          <div className="small">
                            <a
                              href={`${url}/${rp?.full_name || r.full_name}`}
                              target="_blank"
                              rel="noreferrer noopener"
                            >
                              Open in Forgejo
                            </a>
                          </div>
                        )}
                      </td>
                      <td>{rp?.present ? rp.default_branch || "–" : "–"}</td>
                      <td className="mono">
                        {rp?.present ? (
                          rp.head_error ? (
                            <span title={rp.head_error}>unreadable</span>
                          ) : (
                            rp.head_sha.slice(0, 12) || "–"
                          )
                        ) : (
                          "–"
                        )}
                      </td>
                      <td>
                        {rp?.present
                          ? rp.private
                            ? "Private"
                            : "Public"
                          : "–"}
                      </td>
                      <td>{flags.length ? flags.join(", ") : "–"}</td>
                      <td
                        className="num"
                        title={
                          rp?.forgejo_updated_at
                            ? formatDateTime(rp.forgejo_updated_at)
                            : undefined
                        }
                      >
                        {rp?.present && rp.forgejo_updated_at
                          ? formatAgo(rp.forgejo_updated_at, now)
                          : "–"}
                      </td>
                      <td
                        className="num"
                        title={
                          rp?.last_seen_at
                            ? formatDateTime(rp.last_seen_at)
                            : undefined
                        }
                      >
                        {rp?.last_seen_at
                          ? formatAgo(rp.last_seen_at, now)
                          : "–"}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
          <p className="muted small">
            ForgeSync ID <span className="mono">{r.id}</span>, first found{" "}
            {formatDateTime(r.first_seen_at)}.
          </p>
        </>
      )}
    </>
  );
}

function PrimaryPanel({
  repo,
  onSaved,
}: {
  repo: Repository;
  onSaved: () => void;
}) {
  const session = useSession();
  const canEdit = hasRole(session, "administrator");
  const [choice, setChoice] = useState(repo.primary_node);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<{ ok: boolean; text: string }>();

  const { readOnly, leaderName } = useLeadership();
  useEffect(() => setChoice(repo.primary_node), [repo.primary_node]);

  async function save() {
    setBusy(true);
    setMessage(undefined);
    try {
      await api.setPrimary(repo.id, choice);
      setMessage({ ok: true, text: `Primary set to ${choice}.` });
      onSaved();
    } catch (e) {
      setMessage({
        ok: false,
        text: e instanceof ApiError ? e.message : "Saving failed.",
      });
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="panel" aria-labelledby="primary-heading">
      <h2 id="primary-heading">Primary node</h2>
      <p className="muted">
        The node that's authoritative for this repository. By default it's the
        owner's primary site (where they registered); for organizations, the
        node the repository was created on first.{" "}
        {repo.replication?.enabled
          ? "Replication copies its branches and tags to the other nodes; changing it changes where they're copied from."
          : "While replication is off, it's only recorded."}
      </p>
      {canEdit ? (
        <div className="toolbar">
          <label htmlFor="primary">Primary</label>
          <select
            id="primary"
            value={choice}
            onChange={(e) => setChoice(e.target.value)}
          >
            {!repo.primary_node && <option value="">Not set yet</option>}
            {repo.nodes.map((v) => (
              <option key={v.node} value={v.node}>
                {v.node}
                {v.presence !== "present" ? " (doesn't have it)" : ""}
              </option>
            ))}
          </select>
          <button
            type="button"
            className="button-primary"
            onClick={save}
            disabled={
              busy || !choice || choice === repo.primary_node || readOnly
            }
            title={
              readOnly
                ? `Only ${leaderName || "the controller in charge"} can set the primary`
                : undefined
            }
          >
            {busy ? "Saving…" : "Save"}
          </button>
          <PrimarySource repo={repo} />
          {message && (
            <span
              role="status"
              className={message.ok ? "muted" : "error-inline"}
            >
              {message.text}
            </span>
          )}
        </div>
      ) : (
        <p>
          <strong>{repo.primary_node || "Not set yet"}</strong>{" "}
          <PrimarySource repo={repo} />
          <span className="muted"> · Only administrators can change it.</span>
        </p>
      )}
    </section>
  );
}

function IssuesPanel({
  repositoryId,
  nodeNames,
}: {
  repositoryId: string;
  nodeNames: string[];
}) {
  const list = useLoad(
    () => api.repositoryIssues(repositoryId),
    [repositoryId],
  );
  const issues = list.data?.issues ?? [];
  if (list.error || issues.length === 0) return null;
  const differ = issues.filter((i) => i.numbers_differ).length;
  return (
    <section className="panel" aria-labelledby="issues-heading">
      <h2 id="issues-heading">Issues</h2>
      <p className="muted">
        {issues.length} issue{issues.length === 1 ? "" : "s"} replicated, with
        their comments. Forgejo numbers issues itself, so ForgeSync copies them
        in the order they were created; the number stays the same where it can.
        {differ > 0 &&
          ` ${differ} ${differ === 1 ? "has" : "have"} a different number on some node (a pull request usually took it), so "#" references in text can point elsewhere there.`}
      </p>
      <div className="table-wrap flat">
        <table>
          <thead>
            <tr>
              <th scope="col">Issue</th>
              <th scope="col">State</th>
              {nodeNames.map((n) => (
                <th scope="col" key={n}>
                  {n}
                </th>
              ))}
              <th scope="col">Comments</th>
            </tr>
          </thead>
          <tbody>
            {issues.map((i) => (
              <tr key={i.id}>
                <th scope="row">
                  {i.title}
                  <div className="small muted">
                    by {i.author}, opened on {i.origin_node}
                    {i.deleted_at && " · deleted on the primary"}
                  </div>
                </th>
                <td>{i.state === "closed" ? "Closed" : "Open"}</td>
                {nodeNames.map((n) => (
                  <td key={n} className="num">
                    {i.copies[n] ? (
                      `#${i.copies[n].number}`
                    ) : (
                      <span className="muted">–</span>
                    )}
                  </td>
                ))}
                <td className="num">{i.comments}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}

function ArchivePanel({ repo }: { repo: Repository }) {
  const kept = (repo.archives ?? []).filter((a) => a.state !== "purged");
  return (
    <section className="panel" aria-labelledby="deleted-heading">
      <h2 id="deleted-heading">
        {repo.deleted_at ? "Deleted on the primary" : "Archived copies"}
      </h2>
      {repo.deleted_at ? (
        <p>
          Deleted on {repo.primary_node} (found{" "}
          {formatDateTime(repo.deleted_at)}). ForgeSync doesn't delete the other
          copies right away: it moves each one into a private archive
          organization, with its issues, wiki and all commits, and deletes it
          after the backup period. An administrator can move a copy back to
          restore it.
        </p>
      ) : (
        <p>
          This repository was deleted on its primary once and created again.
          These copies of the old one are kept until they expire.
        </p>
      )}
      {kept.length > 0 ? (
        <ul>
          {kept.map((a) => (
            <li key={a.id}>
              {a.node}: <span className="mono">{a.archived_name}</span>
              {a.state === "archived" && a.delete_after
                ? ` in the archive organization · deleted after ${formatDateTime(a.delete_after)}`
                : " · being archived"}
            </li>
          ))}
        </ul>
      ) : (
        <p className="muted">No copies are kept.</p>
      )}
    </section>
  );
}

function PrimarySource({ repo }: { repo: Repository }) {
  if (!repo.primary_node) {
    return (
      <span className="muted">
        Set automatically after the next complete scan of every node.
      </span>
    );
  }
  return (
    <span className="muted">
      {repo.primary_source === "owner"
        ? "Set automatically: the owner's primary site."
        : repo.primary_source === "origin"
          ? "Set automatically: created here first."
          : "Chosen by an administrator; it no longer follows the owner."}
    </span>
  );
}

function RepoConflicts({ repositoryId }: { repositoryId: string }) {
  const open = useLoad(
    () =>
      api.conflicts({
        state: "open",
        repository: repositoryId,
        limit: 20,
        offset: 0,
      }),
    [repositoryId],
  );
  if (!open.data?.items?.length) return null;
  return (
    <section
      className="panel conflict-panel"
      aria-labelledby="repo-conflicts-heading"
    >
      <h2 id="repo-conflicts-heading">
        <StatusIcon tone="serious" /> Open conflicts
      </h2>
      <ul>
        {open.data.items.map((c) => (
          <li key={c.id}>
            <Link to={`/conflicts/${c.id}`}>{conflictTitle(c)}</Link>
            <span className="muted">
              {" "}
              · detected {formatDateTime(c.detected_at)}
            </span>
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
  archived: { tone: "neutral", label: "Archived" },
};

function ReplicationPanel({
  repo,
  onChange,
}: {
  repo: Repository;
  onChange: () => void;
}) {
  const session = useSession();
  const now = useNow(5000);
  const [message, setMessage] = useState<{ ok: boolean; text: string }>();
  const [polling, setPolling] = useState(false);
  const { readOnly, leaderName } = useLeadership();
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
      setMessage({
        ok: true,
        text: r.queued
          ? "Replication started."
          : "Replication is already running.",
      });
      setPolling(true);
    } catch (e) {
      setMessage({
        ok: false,
        text: e instanceof ApiError ? e.message : "Couldn't start replication.",
      });
    }
  }

  if (!repl?.enabled) {
    return (
      <section className="panel" aria-labelledby="repl-heading">
        <h2 id="repl-heading">Replication</h2>
        <p className="muted">
          Replication is turned off on this controller (replication.enabled in
          its config).
        </p>
      </section>
    );
  }
  return (
    <section className="panel" aria-labelledby="repl-heading">
      <div className="section-header">
        <h2 id="repl-heading">Replication</h2>
        {repo.primary_node && hasRole(session, "operator") && (
          <button
            type="button"
            className="button-quiet"
            onClick={replicate}
            disabled={polling || readOnly}
            title={
              readOnly
                ? `Only ${leaderName || "the controller in charge"} can start replication`
                : undefined
            }
          >
            {polling ? "Replicating…" : "Replicate now"}
          </button>
        )}
      </div>
      {!repo.primary_node ? (
        <p className="muted">
          Replication starts once the primary is set, after the next complete
          scan.
        </p>
      ) : repl.replicas.length === 0 ? (
        <p className="muted">
          Branches and tags are copied from {repo.primary_node} after each scan.
          It hasn't run for this repository yet.
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
                const info = REPLICA_STATE[x.state] ?? {
                  tone: "neutral" as Tone,
                  label: x.state,
                };
                return (
                  <tr key={x.node}>
                    <th scope="row">{x.node}</th>
                    <td>
                      <span className="status-badge">
                        <StatusIcon tone={info.tone} />
                        <span>{info.label}</span>
                      </span>
                      {x.detail && (
                        <div className="small replica-detail">{x.detail}</div>
                      )}
                      {x.state === "conflict" && (
                        <div className="small">
                          <Link to="/conflicts">See conflicts</Link>
                        </div>
                      )}
                    </td>
                    <td
                      className="num"
                      title={
                        x.last_success_at
                          ? formatDateTime(x.last_success_at)
                          : undefined
                      }
                    >
                      {x.last_success_at
                        ? formatAgo(x.last_success_at, now)
                        : "Never"}
                    </td>
                    <td className="num">
                      {x.out_of_sync_since
                        ? formatDuration(now - Date.parse(x.out_of_sync_since))
                        : "–"}
                    </td>
                    <td
                      className="num"
                      title={formatDateTime(x.last_attempt_at)}
                    >
                      {formatAgo(x.last_attempt_at, now)}
                      {x.refs_updated > 0 && (
                        <div className="small muted">
                          {x.refs_updated} ref(s) updated
                        </div>
                      )}
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
        Only fast-forwards, new refs and deletions of what ForgeSync wrote
        itself are copied. Anything else becomes a conflict. Missing
        repositories aren't created on replicas yet.
      </p>
    </section>
  );
}
