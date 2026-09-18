import { useState } from "react";
import { api, type AuditEntry } from "../api";
import { ErrorNote, PageHeader } from "../components/Layout";
import { formatDateTime } from "../format";
import { useLoad } from "../hooks";

const PAGE = 100;

export function Audit() {
  const first = useLoad(() => api.audit(PAGE));
  const [older, setOlder] = useState<AuditEntry[]>([]);
  const [olderDone, setOlderDone] = useState(false);
  const [olderError, setOlderError] = useState<string>();
  const [filter, setFilter] = useState("");

  const entries = [...(first.data ?? []), ...older];
  const done = olderDone || (first.data !== undefined && first.data.length < PAGE);
  const q = filter.trim().toLowerCase();
  const shown = q
    ? entries.filter((e) =>
        [e.actor, e.action, e.target, JSON.stringify(e.details)].some((f) => f.toLowerCase().includes(q)),
      )
    : entries;

  async function loadOlder() {
    const last = entries[entries.length - 1];
    if (!last) return;
    try {
      const page = await api.audit(PAGE, last.id);
      setOlder((o) => [...o, ...page]);
      if (page.length < PAGE) setOlderDone(true);
      setOlderError(undefined);
    } catch (e) {
      setOlderError(e instanceof Error ? e.message : String(e));
    }
  }

  function refresh() {
    setOlder([]);
    setOlderDone(false);
    first.reload();
  }

  return (
    <>
      <PageHeader title="Audit log">
        <button type="button" className="button-quiet" onClick={refresh}>
          Refresh
        </button>
      </PageHeader>
      <p className="muted page-intro">Sign-ins and administrative actions, newest first.</p>

      <div className="toolbar">
        <label htmlFor="audit-filter">Filter</label>
        <input
          id="audit-filter"
          type="search"
          placeholder="Actor, action or target"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
        />
        {q && (
          <span className="muted" aria-live="polite">
            {shown.length} of {entries.length} loaded entries
          </span>
        )}
      </div>

      {first.error && <ErrorNote message={first.error} />}
      {first.data && entries.length === 0 && <p className="muted">The audit log is empty.</p>}
      {shown.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">When</th>
                <th scope="col">Actor</th>
                <th scope="col">Action</th>
                <th scope="col">Target</th>
                <th scope="col">Details</th>
              </tr>
            </thead>
            <tbody>
              {shown.map((e) => (
                <tr key={e.id}>
                  <td className="num">
                    <time dateTime={e.at}>{formatDateTime(e.at)}</time>
                  </td>
                  <td>{e.actor}</td>
                  <td className="mono">{e.action}</td>
                  <td>{e.target || "–"}</td>
                  <td className="mono details">
                    {Object.keys(e.details ?? {}).length ? JSON.stringify(e.details) : "–"}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {olderError && <ErrorNote message={olderError} />}
      {!done && entries.length > 0 && (
        <button type="button" className="button-quiet load-more" onClick={loadOlder}>
          Load older entries
        </button>
      )}
    </>
  );
}
