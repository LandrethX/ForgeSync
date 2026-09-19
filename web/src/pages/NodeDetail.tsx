import { useEffect } from "react";
import { api } from "../api";
import { ErrorNote, PageHeader } from "../components/Layout";
import { StatusBadge, stateInfo } from "../components/StatusBadge";
import { formatAgo, formatDateTime, formatDuration } from "../format";
import { useLoad, useNodes, useNow } from "../hooks";
import { Link } from "../router";

export function NodeDetail({ name }: { name: string }) {
  const { nodes } = useNodes();
  const now = useNow();
  const node = nodes?.find((n) => n.name === name);
  const history = useLoad(() => api.transitions(name, 100), [name]);

  // New state -> new history row.
  const { reload } = history;
  useEffect(() => {
    if (node?.state) reload();
  }, [node?.state, reload]);

  if (nodes && !node) {
    return (
      <>
        <PageHeader title="Node not found" />
        <p>
          There's no node called <strong>{name}</strong>.{" "}
          <Link to="/nodes">Back to nodes</Link>
        </p>
      </>
    );
  }

  return (
    <>
      <p className="breadcrumb">
        <Link to="/nodes">Nodes</Link> / {name}
      </p>
      <PageHeader title={name}>
        {node && <StatusBadge state={node.state} />}
      </PageHeader>
      {node && (
        <p className="muted page-intro">{stateInfo(node.state).description}</p>
      )}

      {node && (
        <dl className="facts facts-wide">
          <dt>URL</dt>
          <dd>
            <a href={node.url} target="_blank" rel="noreferrer noopener">
              {node.url}
            </a>
          </dd>
          <dt>Site</dt>
          <dd>{node.site || "–"}</dd>
          <dt>Forgejo version</dt>
          <dd>{node.version || "–"}</dd>
          <dt>Last checked</dt>
          <dd
            title={
              node.last_checked ? formatDateTime(node.last_checked) : undefined
            }
          >
            {node.state === "UNKNOWN" ? "–" : formatAgo(node.last_checked, now)}
          </dd>
          <dt>Last seen</dt>
          <dd
            title={node.last_seen ? formatDateTime(node.last_seen) : undefined}
          >
            {formatAgo(node.last_seen, now)}
          </dd>
          {node.failing_since && (
            <>
              <dt>Not healthy for</dt>
              <dd title={`Since ${formatDateTime(node.failing_since)}`}>
                {formatDuration(now - Date.parse(node.failing_since))}
              </dd>
            </>
          )}
          {node.consecutive_failures > 0 && (
            <>
              <dt>Failed checks in a row</dt>
              <dd>{node.consecutive_failures}</dd>
            </>
          )}
          {node.last_error && (
            <>
              <dt>Last error</dt>
              <dd className="mono">{node.last_error}</dd>
            </>
          )}
        </dl>
      )}

      <section aria-labelledby="history-heading">
        <h2 id="history-heading">State history</h2>
        {history.error && <ErrorNote message={history.error} />}
        {history.data && history.data.length === 0 && (
          <p className="muted">No state changes recorded yet.</p>
        )}
        {history.data && history.data.length > 0 && (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th scope="col">When</th>
                  <th scope="col">From</th>
                  <th scope="col">To</th>
                  <th scope="col">Error</th>
                </tr>
              </thead>
              <tbody>
                {history.data.map((t) => (
                  <tr key={t.id}>
                    <td className="num">
                      <time dateTime={t.at}>{formatDateTime(t.at)}</time>
                    </td>
                    <td>{stateInfo(t.from).label}</td>
                    <td>
                      <StatusBadge state={t.to} />
                    </td>
                    <td className="mono">{t.error || "–"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </>
  );
}
