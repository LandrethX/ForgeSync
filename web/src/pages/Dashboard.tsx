import { useEffect, useMemo } from "react";
import { api, type Node } from "../api";
import { ErrorNote, PageHeader } from "../components/Layout";
import { StatusBadge, StatusIcon, stateInfo } from "../components/StatusBadge";
import { formatAgo, formatDateTime, formatDuration } from "../format";
import { useLoad, useNodes, useNow } from "../hooks";
import { Link } from "../router";

export function Dashboard() {
  const { nodes } = useNodes();
  const now = useNow();
  const overview = useLoad(() => api.overview());
  const changes = useLoad(() => api.transitions(null, 8));

  // Reload the summaries whenever a node changes state (not on every check).
  const stateKey = useMemo(() => (nodes ?? []).map((n) => `${n.name}:${n.state}`).join(","), [nodes]);
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
  const problems = nodes?.filter((n) => n.state !== "HEALTHY" && n.state !== "UNKNOWN") ?? [];

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
            <StatusIcon tone={problems.some((n) => n.state === "UNREACHABLE") ? "critical" : "warning"} />
            Needs attention: {problems.map((n) => n.name).join(", ")}
          </p>
        ) : (
          nodes && total > 0 && <p className="hero-detail muted">All checked nodes are healthy.</p>
        )}
      </section>

      <section className="tiles" aria-label="Controller">
        <div className="tile">
          <p className="tile-label">Database</p>
          {overview.data ? (
            <p className="tile-value">
              <StatusIcon tone={overview.data.database.ok ? "good" : "critical"} />
              {overview.data.database.ok ? "Connected" : "Unreachable"}
            </p>
          ) : (
            <p className="tile-value muted">–</p>
          )}
        </div>
        <div className="tile">
          <p className="tile-label">Controller</p>
          <p className="tile-value">{overview.data?.role === "single" ? "Single controller" : overview.data?.role ?? "–"}</p>
          <p className="tile-note">Leader election comes with high availability</p>
        </div>
        <div className="tile">
          <p className="tile-label">Running for</p>
          <p className="tile-value">
            {overview.data ? formatDuration(now - Date.parse(overview.data.started_at)) : "–"}
          </p>
          {overview.data && (
            <p className="tile-note">
              Version {overview.data.version} ({overview.data.commit})
            </p>
          )}
        </div>
      </section>
      {overview.error && <ErrorNote message={`Couldn't load the summary: ${overview.error}`} />}

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
        {changes.data && changes.data.length === 0 && <p className="muted">No state changes recorded yet.</p>}
        {changes.data && changes.data.length > 0 && (
          <ol className="change-list">
            {changes.data.map((t) => (
              <li key={t.id}>
                <time dateTime={t.at} title={formatDateTime(t.at)}>
                  {formatAgo(t.at, now)}
                </time>
                <Link to={`/nodes/${encodeURIComponent(t.node)}`} className="change-node">
                  {t.node}
                </Link>
                <span className="change-states">
                  {stateInfo(t.from).label} <span aria-label="to">→</span> <StatusBadge state={t.to} />
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
  const down = node.failing_since ? formatDuration(now - Date.parse(node.failing_since)) : undefined;
  return (
    <li className="node-card">
      <div className="node-card-head">
        <span className="node-card-name">
          <Link to={`/nodes/${encodeURIComponent(node.name)}`}>{node.name}</Link>
          {node.site && <span className="muted"> · {node.site}</span>}
        </span>
        <StatusBadge state={node.state} />
      </div>
      <dl className="facts">
        <dt>Last seen</dt>
        <dd title={node.last_seen ? formatDateTime(node.last_seen) : undefined}>{formatAgo(node.last_seen, now)}</dd>
        {down && node.state !== "HEALTHY" && (
          <>
            <dt>Not healthy for</dt>
            <dd>{down}</dd>
          </>
        )}
        <dt>Version</dt>
        <dd>{node.version || "–"}</dd>
      </dl>
      {node.last_error && node.state !== "HEALTHY" && <p className="node-card-error">{node.last_error}</p>}
    </li>
  );
}
