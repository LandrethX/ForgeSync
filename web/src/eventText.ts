import type { HistoryEvent, NodeState } from "./api";
import { stateInfo } from "./components/StatusBadge";

/** Filter labels for the categories ForgeSync writes today; others show as-is. */
export const CATEGORIES: { key: string; label: string }[] = [
  { key: "session", label: "Sign-ins" },
  { key: "node", label: "Node health" },
  { key: "repo", label: "Repositories" },
  { key: "conflict", label: "Conflicts" },
  { key: "inventory", label: "Scans" },
  { key: "history", label: "Exports" },
];

export function categoryLabel(key: string): string {
  return CATEGORIES.find((c) => c.key === key)?.label ?? key;
}

const str = (v: unknown) => (typeof v === "string" ? v : "");

/** A readable sentence for an event; falls back to the raw action. */
export function describe(e: HistoryEvent): string {
  const d = e.details ?? {};
  switch (e.action) {
    case "session.sign_in":
      return d.break_glass ? "Signed in with the admin token (break-glass)" : "Signed in";
    case "session.sign_in_failed":
      return "Sign-in failed: wrong admin token";
    case "session.sign_in_denied":
      return "Sign-in refused: no ForgeSync role in SceneID";
    case "session.sign_out":
      return "Signed out";
    case "repo.set_primary": {
      const from = str(d.from) || "not set";
      const to = str(d.to) || "not set";
      return `Primary changed from ${from} to ${to}`;
    }
    case "inventory.scan_requested":
      return "Asked for a repository scan";
    case "conflict.opened":
      return `Conflict detected: ${kindText(str(d.kind))}`;
    case "conflict.cleared":
      return `Conflict cleared: ${kindText(str(d.kind))}`;
    case "conflict.acknowledged":
      return str(d.note) ? `Acknowledged a conflict: “${str(d.note)}”` : "Acknowledged a conflict";
    case "node.state_changed":
      return `${stateInfo(str(d.from) as NodeState).label} → ${stateInfo(str(d.to) as NodeState).label}`;
    case "history.exported":
      return `Exported the history as ${str(d.format).toUpperCase() || "a file"}`;
  }
  return e.action;
}

function kindText(kind: string): string {
  if (kind === "git_diverged") return "diverged history";
  if (kind === "default_branch_mismatch") return "different default branches";
  return kind;
}

/** Where the event's target lives in the UI, if anywhere. */
export function targetLink(e: HistoryEvent): string | undefined {
  const d = e.details ?? {};
  if (e.category === "node" && e.target) return `/nodes/${encodeURIComponent(e.target)}`;
  if (e.category === "conflict" && typeof d.conflict_id === "number") return `/conflicts/${d.conflict_id}`;
  if (e.category === "repo" && str(d.repository_id)) return `/repositories/${str(d.repository_id)}`;
  return undefined;
}
