import { api, type NodeWebhook } from "../api";
import { PageHeader } from "../components/Layout";
import { StatusBadge, StatusIcon } from "../components/StatusBadge";
import { formatAgo, formatDateTime } from "../format";
import { useLoad, useNodes, useNow } from "../hooks";
import { Link } from "../router";

export function Nodes() {
  const { nodes } = useNodes();
  const now = useNow();
  const hooks = useLoad(() => api.webhooks(), []);
  const showHooks = hooks.data?.enabled === true;

  return (
    <>
      <PageHeader title="Nodes" />
      <p className="muted page-intro">
        Nodes come from the controller's config file. Their health is checked on
        the configured interval and updates here live.
        {showHooks &&
          " Each node reports changes to ForgeSync through a system webhook, so replication starts within seconds; the regular scan is a safety net."}
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
                {showHooks && <th scope="col">Webhook</th>}
                <th scope="col">URL</th>
              </tr>
            </thead>
            <tbody>
              {nodes.map((n) => (
                <tr key={n.name}>
                  <th scope="row">
                    <Link to={`/nodes/${encodeURIComponent(n.name)}`}>
                      {n.name}
                    </Link>
                  </th>
                  <td>{n.site || "–"}</td>
                  <td>
                    <StatusBadge state={n.state} />
                  </td>
                  <td>{n.version || "–"}</td>
                  <td
                    className="num"
                    title={
                      n.last_seen ? formatDateTime(n.last_seen) : undefined
                    }
                  >
                    {formatAgo(n.last_seen, now)}
                  </td>
                  <td
                    className="num"
                    title={
                      n.last_checked
                        ? formatDateTime(n.last_checked)
                        : undefined
                    }
                  >
                    {n.state === "UNKNOWN"
                      ? "–"
                      : formatAgo(n.last_checked, now)}
                  </td>
                  {showHooks && (
                    <td>
                      <WebhookCell
                        hook={hooks.data?.nodes.find((h) => h.node === n.name)}
                        now={now}
                      />
                    </td>
                  )}
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

function WebhookCell({ hook, now }: { hook?: NodeWebhook; now: number }) {
  if (!hook) return <>–</>;
  if (hook.error && !hook.installed) {
    return (
      <span className="status-badge tone-critical" title={hook.error}>
        <StatusIcon tone="critical" />
        <span>Not installed</span>
      </span>
    );
  }
  if (!hook.installed) return <span className="muted">Checking…</span>;
  return (
    <>
      <span className="status-badge tone-good" title={hook.error || undefined}>
        <StatusIcon tone="good" />
        <span>Installed</span>
      </span>
      <div className="small muted">
        {hook.last_delivery_at
          ? `Last delivery ${formatAgo(hook.last_delivery_at, now)} (${hook.deliveries} so far)`
          : "No deliveries yet"}
        {hook.rejected > 0 && ` · ${hook.rejected} rejected`}
      </div>
    </>
  );
}
