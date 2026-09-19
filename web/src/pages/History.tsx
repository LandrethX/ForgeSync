import { useEffect, useMemo, useState } from "react";
import {
  api,
  historyExportURL,
  type HistoryEvent,
  type HistoryFilter,
} from "../api";
import { ErrorNote, PageHeader } from "../components/Layout";
import { CATEGORIES, categoryLabel, describe, targetLink } from "../eventText";
import { formatAgo, formatDateTime } from "../format";
import { useDebounced, useLoad, useNow } from "../hooks";
import { Link } from "../router";

const PAGE = 100;

type Range = "1h" | "24h" | "7d" | "30d" | "all" | "custom";
const RANGES: { key: Range; label: string; ms?: number }[] = [
  { key: "1h", label: "Last hour", ms: 3600e3 },
  { key: "24h", label: "Last 24 hours", ms: 86400e3 },
  { key: "7d", label: "Last 7 days", ms: 7 * 86400e3 },
  { key: "30d", label: "Last 30 days", ms: 30 * 86400e3 },
  { key: "all", label: "All time" },
  { key: "custom", label: "Custom dates" },
];

interface Filters {
  categories: string[];
  actor: string;
  q: string;
  range: Range;
  fromDate: string; // yyyy-mm-dd, local, for "custom"
  toDate: string;
}

/** Filters live in the address bar, so a filtered view can be shared or bookmarked. */
function readFilters(): Filters {
  const p = new URLSearchParams(window.location.search);
  const range = (p.get("range") as Range) || "7d";
  return {
    categories: (p.get("category") ?? "").split(",").filter(Boolean),
    actor: p.get("actor") ?? "",
    q: p.get("q") ?? "",
    range: RANGES.some((r) => r.key === range) ? range : "7d",
    fromDate: p.get("from") ?? "",
    toDate: p.get("to") ?? "",
  };
}

function writeFilters(f: Filters) {
  const p = new URLSearchParams();
  if (f.categories.length) p.set("category", f.categories.join(","));
  if (f.actor) p.set("actor", f.actor);
  if (f.q) p.set("q", f.q);
  if (f.range !== "7d") p.set("range", f.range);
  if (f.range === "custom") {
    if (f.fromDate) p.set("from", f.fromDate);
    if (f.toDate) p.set("to", f.toDate);
  }
  const qs = p.toString();
  window.history.replaceState(
    null,
    "",
    window.location.pathname + (qs ? `?${qs}` : ""),
  );
}

/** Turns the page's filters into the API's (times in UTC). */
function toApiFilter(f: Filters, now: number): HistoryFilter {
  const out: HistoryFilter = {
    categories: f.categories,
    actor: f.actor,
    q: f.q,
  };
  const preset = RANGES.find((r) => r.key === f.range);
  if (preset?.ms) out.from = new Date(now - preset.ms).toISOString();
  if (f.range === "custom") {
    // Dates are the viewer's local days; "to" includes the whole day.
    if (f.fromDate) out.from = new Date(`${f.fromDate}T00:00:00`).toISOString();
    if (f.toDate) {
      const end = new Date(`${f.toDate}T00:00:00`);
      end.setDate(end.getDate() + 1);
      out.to = end.toISOString();
    }
  }
  return out;
}

export function History() {
  const [filters, setFilters] = useState<Filters>(readFilters);
  const q = useDebounced(filters.q.trim(), 300);
  const effective = useMemo(() => ({ ...filters, q }), [filters, q]);
  // Preset ranges are relative to when the filter was set, not every render.
  const [anchor, setAnchor] = useState(() => Date.now());
  const apiFilter = useMemo(
    () => toApiFilter(effective, anchor),
    [effective, anchor],
  );
  const key = JSON.stringify(apiFilter);

  useEffect(() => writeFilters(filters), [filters]);

  const actors = useLoad(() => api.historyActors());
  const first = useLoad(() => api.history(apiFilter, PAGE), [key]);
  const [more, setMore] = useState<HistoryEvent[]>([]);
  const [cursor, setCursor] = useState<string>();
  const [moreError, setMoreError] = useState<string>();
  const [loadingMore, setLoadingMore] = useState(false);
  const now = useNow(15000);

  useEffect(() => {
    setMore([]);
    setCursor(first.data?.next_cursor);
    setMoreError(undefined);
  }, [first.data]);

  const events = [...(first.data?.items ?? []), ...more];
  const update = (patch: Partial<Filters>) => {
    setFilters((f) => ({ ...f, ...patch }));
    setAnchor(Date.now());
  };
  const toggleCategory = (c: string) =>
    update({
      categories: filters.categories.includes(c)
        ? filters.categories.filter((x) => x !== c)
        : [...filters.categories, c],
    });

  async function loadMore() {
    if (!cursor) return;
    setLoadingMore(true);
    try {
      const page = await api.history(apiFilter, PAGE, cursor);
      setMore((m) => [...m, ...page.items]);
      setCursor(page.next_cursor);
      setMoreError(undefined);
    } catch (e) {
      setMoreError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoadingMore(false);
    }
  }

  const filtered =
    filters.categories.length > 0 ||
    filters.actor ||
    filters.q ||
    filters.range !== "all";

  return (
    <>
      <PageHeader title="Events & audit">
        <div className="toolbar tight">
          <button
            type="button"
            className="button-quiet"
            onClick={() => {
              setAnchor(Date.now());
              first.reload();
            }}
          >
            Refresh
          </button>
          <a
            className="button-quiet button-link-inline"
            href={historyExportURL(apiFilter, "csv")}
            download
          >
            Export CSV
          </a>
          <a
            className="button-quiet button-link-inline"
            href={historyExportURL(apiFilter, "json")}
            download
          >
            Export JSON
          </a>
        </div>
      </PageHeader>
      <p className="muted page-intro">
        Sign-ins, administrative actions, conflicts and node health changes,
        newest first. Exports use the filters below and are themselves recorded
        here.
      </p>

      <div className="filters">
        <div className="segmented" role="group" aria-label="Categories">
          {CATEGORIES.map((c) => (
            <button
              key={c.key}
              type="button"
              aria-pressed={filters.categories.includes(c.key)}
              onClick={() => toggleCategory(c.key)}
            >
              {c.label}
            </button>
          ))}
        </div>
        <div className="toolbar">
          <label htmlFor="h-q">Search</label>
          <input
            id="h-q"
            type="search"
            placeholder="Name, repository, address…"
            value={filters.q}
            onChange={(e) => update({ q: e.target.value })}
          />
          <label htmlFor="h-actor">Who</label>
          <select
            id="h-actor"
            value={filters.actor}
            onChange={(e) => update({ actor: e.target.value })}
          >
            <option value="">Anyone</option>
            {(actors.data ?? []).map((a) => (
              <option key={a} value={a}>
                {a}
              </option>
            ))}
          </select>
          <label htmlFor="h-range">When</label>
          <select
            id="h-range"
            value={filters.range}
            onChange={(e) => update({ range: e.target.value as Range })}
          >
            {RANGES.map((r) => (
              <option key={r.key} value={r.key}>
                {r.label}
              </option>
            ))}
          </select>
          {filters.range === "custom" && (
            <>
              <label htmlFor="h-from" className="sr-only">
                From
              </label>
              <input
                id="h-from"
                type="date"
                value={filters.fromDate}
                onChange={(e) => update({ fromDate: e.target.value })}
              />
              <span aria-hidden="true">–</span>
              <label htmlFor="h-to" className="sr-only">
                To
              </label>
              <input
                id="h-to"
                type="date"
                value={filters.toDate}
                onChange={(e) => update({ toDate: e.target.value })}
              />
            </>
          )}
          {filtered && (
            <button
              type="button"
              className="button-quiet"
              onClick={() =>
                update({
                  categories: [],
                  actor: "",
                  q: "",
                  range: "all",
                  fromDate: "",
                  toDate: "",
                })
              }
            >
              Clear filters
            </button>
          )}
        </div>
      </div>

      {first.error && <ErrorNote message={first.error} />}
      {first.data && events.length === 0 && (
        <p className="muted">Nothing matches these filters.</p>
      )}
      {events.length > 0 && (
        <div className="table-wrap">
          <table className="history">
            <thead>
              <tr>
                <th scope="col">When</th>
                <th scope="col">What</th>
                <th scope="col">Who</th>
                <th scope="col">Target</th>
              </tr>
            </thead>
            <tbody>
              {events.map((e) => {
                const link = targetLink(e);
                const hasDetails =
                  e.details && Object.keys(e.details).length > 0;
                return (
                  <tr key={e.id}>
                    <td className="num">
                      <time dateTime={e.at} title={formatAgo(e.at, now)}>
                        {formatDateTime(e.at)}
                      </time>
                    </td>
                    <td>
                      <span className="category">
                        {categoryLabel(e.category)}
                      </span>{" "}
                      {describe(e)}
                      {hasDetails && (
                        <details className="event-details">
                          <summary>Details</summary>
                          <pre>{JSON.stringify(e.details, null, 2)}</pre>
                        </details>
                      )}
                    </td>
                    <td>{e.actor}</td>
                    <td>
                      {e.target ? (
                        link ? (
                          <Link to={link}>{e.target}</Link>
                        ) : (
                          e.target
                        )
                      ) : (
                        <span className="muted">–</span>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      {moreError && <ErrorNote message={moreError} />}
      {cursor && (
        <button
          type="button"
          className="button-quiet load-more"
          onClick={loadMore}
          disabled={loadingMore}
        >
          {loadingMore ? "Loading…" : "Load older entries"}
        </button>
      )}
    </>
  );
}
