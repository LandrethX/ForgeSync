import { api, type Node, type NodeWebhook, type SourcePair } from "../api";
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
  const sources = useLoad(() => api.replicationSources(), []);

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
      {nodes && sources.data?.enabled && (
        <SentFromEachNode
          nodes={nodes}
          pairs={sources.data.pairs ?? []}
          now={now}
        />
      )}
    </>
  );
}

/**
 * What each node has sent: a row per node it was the primary for, a
 * column per node its copies went to. Reading across a row answers "what
 * has this node sent, and is any of it stuck"; reading down a column
 * answers "what has this node been given".
 */
function SentFromEachNode({
  nodes,
  pairs,
  now,
}: {
  nodes: Node[];
  pairs: SourcePair[];
  now: number;
}) {
  const names = nodes.map((n) => n.name);
  const sending = names.filter((name) => pairs.some((p) => p.from === name));
  const at = new Map(pairs.map((p) => [`${p.from}|${p.to}`, p]));

  return (
    <section aria-labelledby="sent-heading">
      <h2 id="sent-heading">Copied from each node</h2>
      <p className="muted page-intro">
        Where each node acted as the original: its repositories&rsquo; branches,
        tags and everything else are copied from it to the others. A cell says
        when the last copy went through, and how many of that node&rsquo;s
        repositories are in step there.
      </p>
      {sending.length === 0 ? (
        <p className="muted">
          Nothing has been copied yet. A repository is copied from its primary
          once it has one, after the next complete scan.
        </p>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">From</th>
                <th scope="col">Repositories</th>
                {names.map((name) => (
                  <th scope="col" key={name}>
                    To {name}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {sending.map((from) => {
                const row = pairs.filter((p) => p.from === from);
                const repositories = Math.max(
                  ...row.map((p) => p.repositories),
                );
                return (
                  <tr key={from}>
                    <th scope="row">
                      <Link to={`/nodes/${encodeURIComponent(from)}`}>
                        {from}
                      </Link>
                    </th>
                    <td className="num">{repositories}</td>
                    {names.map((to) => (
                      <td key={to} className="num">
                        <SentCell
                          from={from}
                          to={to}
                          pair={at.get(`${from}|${to}`)}
                          now={now}
                        />
                      </td>
                    ))}
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

function SentCell({
  from,
  to,
  pair,
  now,
}: {
  from: string;
  to: string;
  pair?: SourcePair;
  now: number;
}) {
  if (from === to) return <span className="muted">–</span>;
  if (!pair) return <span className="muted">–</span>;
  const behind = pair.repositories - pair.in_sync;
  return (
    <>
      <span
        title={
          pair.last_success_at
            ? formatDateTime(pair.last_success_at)
            : pair.last_attempt_at
              ? `Last tried ${formatDateTime(pair.last_attempt_at)}`
              : undefined
        }
      >
        {pair.last_success_at ? formatAgo(pair.last_success_at, now) : "Never"}
      </span>
      <div className="small muted">
        {behind > 0
          ? `${pair.in_sync} of ${pair.repositories} in step`
          : `${pair.repositories} in step`}
      </div>
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
