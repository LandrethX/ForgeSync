/** Compact duration: "45s", "3m 35s", "2h 5m", "4d 3h". Negative counts as zero. */
export function formatDuration(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  const d = Math.floor(total / 86400);
  const h = Math.floor((total % 86400) / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  if (d > 0) return h > 0 ? `${d}d ${h}h` : `${d}d`;
  if (h > 0) return m > 0 ? `${h}h ${m}m` : `${h}h`;
  if (m > 0) return s > 0 ? `${m}m ${s}s` : `${m}m`;
  return `${s}s`;
}

/** "3m 35s ago", or `never` when there's no timestamp. */
export function formatAgo(iso: string | undefined, now: number, never = "Never"): string {
  if (!iso) return never;
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return never;
  const ms = now - t;
  return ms < 1000 ? "Just now" : `${formatDuration(ms)} ago`;
}

/** Absolute local date and time, for tooltips and history tables. */
export function formatDateTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}
