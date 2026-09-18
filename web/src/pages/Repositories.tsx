import { useEffect, useRef, useState } from "react";
import { api, hasRole, type InventoryStatus, type RepoStatus } from "../api";
import { ErrorNote, PageHeader } from "../components/Layout";
import { PresenceLabel, REPO_STATUS, RepoStatusBadge } from "../components/StatusBadge";
import { formatAgo, formatDateTime, formatDuration } from "../format";
import { useDebounced, useLoad, useNodes, useNow, useSession } from "../hooks";
import { Link } from "../router";

const PAGE = 100;
const FILTERS: (RepoStatus | "")[] = ["", "missing", "differs", "unknown", "same"];

export function Repositories() {
  const session = useSession();
  const { nodes } = useNodes();
  const [query, setQuery] = useState("");
  const [status, setStatus] = useState<RepoStatus | "">("");
  const [offset, setOffset] = useState(0);
  const q = useDebounced(query.trim(), 250);

  useEffect(() => setOffset(0), [q, status]);

  const list = useLoad(
    () => api.repositories({ q: q || undefined, status: status || undefined, limit: PAGE, offset }),
    [q, status, offset],
  );
  const scan = useScanStatus(list.reload);
  const nodeNames = nodes?.map((n) => n.name) ?? list.data?.items[0]?.nodes.map((v) => v.node) ?? [];
  const counts = list.data?.counts ?? {};
  const all = Object.values(counts).reduce((a, b) => a + (b ?? 0), 0);

  return (
    <>
      <PageHeader title="Repositories">
        {hasRole(session, "operator") && (
          <button type="button" className="button-quiet" onClick={scan.start} disabled={scan.status?.running}>
            {scan.status?.running ? "Scanning…" : "Scan now"}
          </button>
        )}
      </PageHeader>
      <p className="muted page-intro">
        What each node has, found by scanning every node
        {scan.status ? ` every ${formatDuration(scan.status.interval_seconds * 1000)}` : " regularly"}. This compares
        the default branch on each node; ForgeSync doesn't replicate anything yet.
      </p>
      <ScanSummary status={scan.status} error={scan.error} />

      <div className="toolbar">
        <label htmlFor="repo-search">Search</label>
        <input
          id="repo-search"
          type="search"
          placeholder="owner/name"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
        />
        <div className="segmented" role="group" aria-label="Filter by status">
          {FILTERS.map((f) => (
            <button
              key={f || "all"}
              type="button"
              aria-pressed={status === f}
              onClick={() => setStatus(f)}
            >
              {f ? REPO_STATUS[f].label : "All"} <span className="count">{f ? (counts[f] ?? 0) : all}</span>
            </button>
          ))}
        </div>
      </div>

      {list.error && <ErrorNote message={list.error} />}
      {list.data && list.data.items.length === 0 && (
        <p className="muted">
          {all === 0 ? "No repositories found yet. The first scan may still be running." : "No repositories match."}
        </p>
      )}
      {list.data && list.data.items.length > 0 && (
        <>
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th scope="col">Repository</th>
                  <th scope="col">Status</th>
                  <th scope="col">Primary</th>
                  {nodeNames.map((n) => (
                    <th scope="col" key={n}>
                      {n}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {list.data.items.map((r) => (
                  <tr key={r.id}>
                    <th scope="row">
                      <Link to={`/repositories/${r.id}`}>{r.full_name}</Link>
                    </th>
                    <td>
                      <RepoStatusBadge status={r.status} />
                    </td>
                    <td>
                      {r.primary_node || <span className="muted">Not set yet</span>}
                      {r.primary_source === "manual" && <span className="muted"> (chosen)</span>}
                    </td>
                    {nodeNames.map((n) => {
                      const v = r.nodes.find((x) => x.node === n);
                      const rp = v?.replica;
                      const detail =
                        v?.presence === "present" && rp
                          ? rp.empty
                            ? "Empty"
                            : rp.head_sha
                              ? rp.head_sha.slice(0, 7)
                              : "Branch unreadable"
                          : undefined;
                      return (
                        <td key={n} className={detail && rp?.head_sha ? "mono" : undefined}>
                          <PresenceLabel presence={v?.presence ?? "unknown"} detail={detail} />
                        </td>
                      );
                    })}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <Pager total={list.data.total} offset={offset} onChange={setOffset} />
        </>
      )}
    </>
  );
}

function Pager({ total, offset, onChange }: { total: number; offset: number; onChange: (o: number) => void }) {
  if (total <= PAGE) return <p className="muted pager">{total} repositories</p>;
  return (
    <div className="pager">
      <span className="muted">
        {offset + 1}–{Math.min(offset + PAGE, total)} of {total}
      </span>
      <button type="button" className="button-quiet" disabled={offset === 0} onClick={() => onChange(offset - PAGE)}>
        Previous
      </button>
      <button
        type="button"
        className="button-quiet"
        disabled={offset + PAGE >= total}
        onClick={() => onChange(offset + PAGE)}
      >
        Next
      </button>
    </div>
  );
}

/** Loads the scan status, polls while a scan runs, and reloads `onFinished` after it. */
function useScanStatus(onFinished: () => void) {
  const [status, setStatus] = useState<InventoryStatus>();
  const [error, setError] = useState<string>();
  const wasRunning = useRef(false);
  const finishedRef = useRef(onFinished);
  finishedRef.current = onFinished;

  const refresh = () =>
    api
      .inventory()
      .then((s) => {
        setStatus(s);
        setError(undefined);
        if (wasRunning.current && !s.running) finishedRef.current();
        wasRunning.current = s.running;
      })
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)));

  useEffect(() => {
    refresh();
    // Poll quickly while scanning, slowly otherwise (scheduled scans).
    const id = window.setInterval(refresh, status?.running ? 2000 : 30000);
    return () => window.clearInterval(id);
  }, [status?.running]);

  const start = () => {
    api
      .scanNow()
      .then(() => {
        wasRunning.current = true;
        setStatus((s) => (s ? { ...s, running: true } : s));
      })
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)));
  };
  return { status, error, start };
}

function ScanSummary({ status, error }: { status?: InventoryStatus; error?: string }) {
  const now = useNow(5000);
  if (error) return <ErrorNote message={`Couldn't load the scan status: ${error}`} />;
  if (!status) return null;
  return (
    <ul className="scan-summary" aria-label="Latest scan per node">
      {status.nodes.length === 0 && <li className="muted">No node has been scanned yet.</li>}
      {status.nodes.map((n) => (
        <li key={n.node}>
          <strong>{n.node}</strong>{" "}
          {n.ok ? (
            <span title={formatDateTime(n.finished_at)}>
              {n.repositories} repositories, scanned {formatAgo(n.finished_at, now).toLowerCase()}
            </span>
          ) : (
            <span>
              latest scan failed {formatAgo(n.finished_at, now).toLowerCase()}
              {n.last_success_at
                ? `; showing what was found ${formatAgo(n.last_success_at, now).toLowerCase()}`
                : "; never scanned successfully"}
              {n.error && <span className="mono scan-error"> {n.error}</span>}
            </span>
          )}
        </li>
      ))}
    </ul>
  );
}
