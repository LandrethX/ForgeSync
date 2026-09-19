import { useEffect, useMemo } from "react";
import { api, type Controller, type Node, type Overview } from "../api";
import { ErrorNote, PageHeader } from "../components/Layout";
import {
  StatusBadge,
  StatusIcon,
  stateInfo,
  type Tone,
} from "../components/StatusBadge";
import { formatAgo, formatDateTime, formatDuration } from "../format";
import { useLoad, useNodes, useNow } from "../hooks";
import { Link } from "../router";

/** How this controller describes itself: single, leading or standing by. */
function controllerRole(role: string | undefined): string {
  switch (role) {
    case "single":
      return "Single controller";
    case "leader":
      return "Leading";
    case "standby":
      return "On standby";
    default:
      return "–";
  }
}

/** Who is doing the work, for a controller that isn't. */
function controllerNote(o: Overview | undefined): string {
  if (!o) return "";
  if (o.role === "single")
    return "One controller, holding the lease on its own";
  if (o.role === "leader") return "This controller is doing the work";
  if (o.leader?.error) return `Leadership unknown: ${o.leader.error}`;
  if (o.leader?.name) {
    return `${o.leader.name} is doing the work; this one takes over if it stops`;
  }
  return "Waiting to take over";
}

/** What each controller's role means, in the words the UI uses elsewhere. */
const CONTROLLER_ROLE: Record<
  Controller["role"],
  { label: string; tone: Tone; note: string }
> = {
  leader: {
    label: "Leader",
    tone: "good",
    note: "Doing the work: scanning, replicating and writing to the nodes",
  },
  standby: {
    label: "Standby",
    tone: "neutral",
    note: "Serving the pages, ready to take over if the leader stops",
  },
  unknown: {
    label: "Not heard from",
    tone: "serious",
    note: "It hasn't checked in lately; it may be stopped",
  },
};

/**
 * A card per ForgeSync controller sharing the database: where it is and
 * what it's doing. The address is what the database sees the connection
 * come from, so it's the controller's real one rather than what it
 * believes about itself.
 */
function Controllers({ list, now }: { list: Controller[]; now: number }) {
  if (list.length === 0) return null;
  return (
    <section aria-labelledby="controllers-heading">
      <div className="section-header">
        <h2 id="controllers-heading">Sync controllers</h2>
      </div>
      <div className="cards">
        {list.map((c) => {
          const role = CONTROLLER_ROLE[c.role] ?? CONTROLLER_ROLE.unknown;
          return (
            <article className="card" key={c.name}>
              <div className="card-head">
                <h3>
                  {c.name}
                  {c.self && <span className="muted"> · this one</span>}
                </h3>
                <span className={`status-badge tone-${role.tone}`}>
                  <StatusIcon tone={role.tone} />
                  <span>{role.label}</span>
                </span>
              </div>
              <dl className="card-rows">
                <dt>Address</dt>
                <dd className="mono">{c.address || "–"}</dd>
                <dt>URL</dt>
                <dd>
                  {c.url ? (
                    <a href={c.url} target="_blank" rel="noreferrer noopener">
                      {c.url}
                    </a>
                  ) : (
                    <span className="muted">Not configured</span>
                  )}
                </dd>
                <dt>Running for</dt>
                <dd>{formatDuration(now - Date.parse(c.started_at))}</dd>
                <dt>Last checked in</dt>
                <dd title={formatDateTime(c.last_seen_at)}>
                  {formatAgo(c.last_seen_at, now)}
                </dd>
              </dl>
              <p className="tile-note">{role.note}</p>
            </article>
          );
        })}
      </div>
    </section>
  );
}

export function Dashboard() {
  const { nodes } = useNodes();
  const now = useNow();
  const overview = useLoad(() => api.overview());
  const changes = useLoad(() => api.transitions(null, 8));

  // Reload the summaries whenever a node changes state (not on every check).
  const stateKey = useMemo(
    () => (nodes ?? []).map((n) => `${n.name}:${n.state}`).join(","),
    [nodes],
  );
  const { reload: reloadOverview } = overview;
  const { reload: reloadChanges } = changes;
  useEffect(() => {
    if (stateKey) {
      reloadOverview();
      reloadChanges();
    }
  }, [stateKey, reloadOverview, reloadChanges]);

  const total = nodes?.length ?? 0;
  const healthy = nodes?.filter((n) => n.state === "HEALTHY").length ?? 0;
  const problems =
    nodes?.filter((n) => n.state !== "HEALTHY" && n.state !== "UNKNOWN") ?? [];

  return (
    <>
      <PageHeader title="Dashboard" />

      <section className="hero" aria-label="Summary">
        <p className="hero-figure">
          {nodes ? `${healthy} of ${total}` : "–"}
          <span className="hero-caption">nodes healthy</span>
        </p>
        {problems.length > 0 ? (
          <p className="hero-detail">
            <StatusIcon
              tone={
                problems.some((n) => n.state === "UNREACHABLE")
                  ? "critical"
                  : "warning"
              }
            />
            Needs attention: {problems.map((n) => n.name).join(", ")}
          </p>
        ) : (
          nodes &&
          total > 0 && (
            <p className="hero-detail muted">All checked nodes are healthy.</p>
          )
        )}
      </section>

      <section className="tiles" aria-label="Controller">
        <div className="tile">
          <p className="tile-label">Database</p>
          {overview.data ? (
            <p className="tile-value">
              <StatusIcon
                tone={overview.data.database.ok ? "good" : "critical"}
              />
              {overview.data.database.ok ? "Connected" : "Unreachable"}
            </p>
          ) : (
            <p className="tile-value muted">–</p>
          )}
        </div>
        <div className="tile">
          <p className="tile-label">Open conflicts</p>
          {overview.data && overview.data.open_conflicts >= 0 ? (
            <p className="tile-value">
              <StatusIcon
                tone={overview.data.open_conflicts > 0 ? "serious" : "good"}
              />
              <Link to="/conflicts">{overview.data.open_conflicts}</Link>
            </p>
          ) : (
            <p className="tile-value muted">–</p>
          )}
          <p className="tile-note">
            Differences between nodes that need a person
          </p>
        </div>
        <div className="tile">
          <p className="tile-label">Replication</p>
          {overview.data ? (
            overview.data.replication.enabled ? (
              <ReplicationTile counts={overview.data.replication.counts} />
            ) : (
              <p className="tile-value muted">Off</p>
            )
          ) : (
            <p className="tile-value muted">–</p>
          )}
        </div>
        <div className="tile">
          <p className="tile-label">This controller</p>
          <p className="tile-value">{controllerRole(overview.data?.role)}</p>
          <p className="tile-note">{controllerNote(overview.data)}</p>
        </div>
        <div className="tile">
          <p className="tile-label">Running for</p>
          <p className="tile-value">
            {overview.data
              ? formatDuration(now - Date.parse(overview.data.started_at))
              : "–"}
          </p>
          {overview.data && (
            <p className="tile-note">
              Version {overview.data.version} ({overview.data.commit})
            </p>
          )}
        </div>
      </section>
      {overview.error && (
        <ErrorNote message={`Couldn't load the summary: ${overview.error}`} />
      )}

      {overview.data && overview.data.controllers !== undefined && (
        <Controllers list={overview.data.controllers} now={now} />
      )}

      <section aria-labelledby="nodes-heading">
        <div className="section-header">
          <h2 id="nodes-heading">Nodes</h2>
          <Link to="/nodes">All details</Link>
        </div>
        {!nodes ? (
          <p className="muted">Waiting for the first health check…</p>
        ) : (
          <ul className="node-cards">
            {nodes.map((n) => (
              <NodeCard key={n.name} node={n} now={now} />
            ))}
          </ul>
        )}
      </section>

      <section aria-labelledby="changes-heading">
        <div className="section-header">
          <h2 id="changes-heading">Recent state changes</h2>
        </div>
        {changes.error && <ErrorNote message={changes.error} />}
        {changes.data && changes.data.length === 0 && (
          <p className="muted">No state changes recorded yet.</p>
        )}
        {changes.data && changes.data.length > 0 && (
          <ol className="change-list">
            {changes.data.map((t) => (
              <li key={t.id}>
                <time dateTime={t.at} title={formatDateTime(t.at)}>
                  {formatAgo(t.at, now)}
                </time>
                <Link
                  to={`/nodes/${encodeURIComponent(t.node)}`}
                  className="change-node"
                >
                  {t.node}
                </Link>
                <span className="change-states">
                  {stateInfo(t.from).label} <span aria-label="to">→</span>{" "}
                  <StatusBadge state={t.to} />
                </span>
                {t.error && <span className="change-error">{t.error}</span>}
              </li>
            ))}
          </ol>
        )}
      </section>
    </>
  );
}

function NodeCard({ node, now }: { node: Node; now: number }) {
  const down = node.failing_since
    ? formatDuration(now - Date.parse(node.failing_since))
    : undefined;
  return (
    <li className="node-card">
      <div className="node-card-head">
        <span className="node-card-name">
          <Link to={`/nodes/${encodeURIComponent(node.name)}`}>
            {node.name}
          </Link>
          {node.site && <span className="muted"> · {node.site}</span>}
        </span>
        <StatusBadge state={node.state} />
      </div>
      <dl className="facts">
        <dt>Last seen</dt>
        <dd title={node.last_seen ? formatDateTime(node.last_seen) : undefined}>
          {formatAgo(node.last_seen, now)}
        </dd>
        {down && node.state !== "HEALTHY" && (
          <>
            <dt>Not healthy for</dt>
            <dd>{down}</dd>
          </>
        )}
        <dt>Version</dt>
        <dd>{node.version || "–"}</dd>
      </dl>
      {node.last_error && node.state !== "HEALTHY" && (
        <p className="node-card-error">{node.last_error}</p>
      )}
    </li>
  );
}

function ReplicationTile({
  counts,
}: {
  counts: Partial<Record<string, number>>;
}) {
  const total = Object.values(counts).reduce((a: number, b) => a + (b ?? 0), 0);
  const synced = counts.synced ?? 0;
  const problems =
    (counts.conflict ?? 0) + (counts.error ?? 0) + (counts.missing ?? 0);
  if (total === 0) {
    return (
      <>
        <p className="tile-value muted">Nothing yet</p>
        <p className="tile-note">Set a primary on a repository to start</p>
      </>
    );
  }
  return (
    <>
      <p className="tile-value">
        <StatusIcon
          tone={
            problems > 0 ? "serious" : synced === total ? "good" : "warning"
          }
        />
        {synced} of {total}
      </p>
      <p className="tile-note">
        replicas in sync
        {problems > 0 ? ` · ${problems} need attention` : ""}
        {counts.waiting ? ` · ${counts.waiting} waiting` : ""}
      </p>
    </>
  );
}
