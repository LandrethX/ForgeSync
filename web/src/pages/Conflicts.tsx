import { useEffect, useState } from "react";
import { api } from "../api";
import { ErrorNote, PageHeader } from "../components/Layout";
import { StatusIcon } from "../components/StatusBadge";
import { conflictSides, conflictTitle } from "../conflictText";
import { formatAgo, formatDateTime } from "../format";
import { useLoad, useNow } from "../hooks";
import { Link } from "../router";

const PAGE = 100;
type Tab = "open" | "cleared";

export function Conflicts() {
  const [tab, setTab] = useState<Tab>("open");
  const [offset, setOffset] = useState(0);
  useEffect(() => setOffset(0), [tab]);
  const list = useLoad(
    () => api.conflicts({ state: tab, limit: PAGE, offset }),
    [tab, offset],
  );
  const now = useNow(10000);
  const counts = list.data?.counts ?? {};

  return (
    <>
      <PageHeader title="Conflicts" />
      <p className="muted page-intro">
        Differences between nodes that ForgeSync won't settle by itself:
        diverged history, commits made on a replica, history rewritten on a
        primary. A conflict clears on its own once a later check finds the nodes
        agree.
      </p>

      <div className="segmented tabs" role="group" aria-label="Show">
        {(["open", "cleared"] as Tab[]).map((t) => (
          <button
            key={t}
            type="button"
            aria-pressed={tab === t}
            onClick={() => setTab(t)}
          >
            {t === "open" ? "Open" : "Cleared"}{" "}
            <span className="count">{counts[t] ?? 0}</span>
          </button>
        ))}
      </div>

      {list.error && <ErrorNote message={list.error} />}
      {list.data && list.data.items.length === 0 && (
        <p className="empty-note">
          {tab === "open" ? (
            <>
              <StatusIcon tone="good" /> No open conflicts.
            </>
          ) : (
            "No conflicts have cleared yet."
          )}
        </p>
      )}
      {list.data && list.data.items.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">Conflict</th>
                <th scope="col">Repository</th>
                <th scope="col">Nodes</th>
                <th scope="col">Primary</th>
                <th scope="col">{tab === "open" ? "Detected" : "Cleared"}</th>
                <th scope="col">Acknowledged</th>
              </tr>
            </thead>
            <tbody>
              {list.data.items.map((c) => {
                const when =
                  tab === "open"
                    ? c.detected_at
                    : (c.cleared_at ?? c.last_seen_at);
                return (
                  <tr key={c.id}>
                    <th scope="row">
                      <span className="presence">
                        <StatusIcon
                          tone={c.state === "open" ? "serious" : "good"}
                        />
                        <Link to={`/conflicts/${c.id}`}>
                          {conflictTitle(c)}
                        </Link>
                      </span>
                    </th>
                    <td>
                      <Link to={`/repositories/${c.repository_id}`}>
                        {c.full_name}
                      </Link>
                    </td>
                    <td className="mono">
                      {conflictSides(c).map(([node, v]) => (
                        <div key={node}>
                          {node}:{" "}
                          {c.kind === "default_branch_mismatch"
                            ? v
                            : v
                              ? v.slice(0, 7)
                              : "–"}
                        </div>
                      ))}
                    </td>
                    <td>
                      {c.primary_node || <span className="muted">Not set</span>}
                    </td>
                    <td className="num" title={formatDateTime(when)}>
                      {formatAgo(when, now)}
                    </td>
                    <td>
                      {c.acknowledged_by ? (
                        <span title={c.note || undefined}>
                          {c.acknowledged_by}
                          {c.note && <span className="muted"> · note</span>}
                        </span>
                      ) : (
                        <span className="muted">No</span>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      {list.data && list.data.total > PAGE && (
        <div className="pager">
          <span className="muted">
            {offset + 1}–{Math.min(offset + PAGE, list.data.total)} of{" "}
            {list.data.total}
          </span>
          <button
            type="button"
            className="button-quiet"
            disabled={offset === 0}
            onClick={() => setOffset(offset - PAGE)}
          >
            Previous
          </button>
          <button
            type="button"
            className="button-quiet"
            disabled={offset + PAGE >= list.data.total}
            onClick={() => setOffset(offset + PAGE)}
          >
            Next
          </button>
        </div>
      )}
    </>
  );
}
