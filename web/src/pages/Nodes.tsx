import { PageHeader } from "../components/Layout";
import { StatusBadge } from "../components/StatusBadge";
import { formatAgo, formatDateTime } from "../format";
import { useNodes, useNow } from "../hooks";
import { Link } from "../router";

export function Nodes() {
  const { nodes } = useNodes();
  const now = useNow();

  return (
    <>
      <PageHeader title="Nodes" />
      <p className="muted page-intro">
        Nodes come from the controller's config file. Their health is checked on the configured interval and updates here live.
      </p>
      {!nodes ? (
        <p className="muted">Waiting for the first health check…</p>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">Node</th>
                <th scope="col">Site</th>
                <th scope="col">Status</th>
                <th scope="col">Version</th>
                <th scope="col">Last seen</th>
                <th scope="col">Last checked</th>
                <th scope="col">URL</th>
              </tr>
            </thead>
            <tbody>
              {nodes.map((n) => (
                <tr key={n.name}>
                  <th scope="row">
                    <Link to={`/nodes/${encodeURIComponent(n.name)}`}>{n.name}</Link>
                  </th>
                  <td>{n.site || "–"}</td>
                  <td>
                    <StatusBadge state={n.state} />
                  </td>
                  <td>{n.version || "–"}</td>
                  <td className="num" title={n.last_seen ? formatDateTime(n.last_seen) : undefined}>
                    {formatAgo(n.last_seen, now)}
                  </td>
                  <td className="num" title={n.last_checked ? formatDateTime(n.last_checked) : undefined}>
                    {n.state === "UNKNOWN" ? "–" : formatAgo(n.last_checked, now)}
                  </td>
                  <td>
                    <a href={n.url} target="_blank" rel="noreferrer noopener">
                      {n.url}
                    </a>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}
