import type { HistoryEvent, NodeState } from "./api";
import { stateInfo } from "./components/StatusBadge";
import { KIND_LABEL } from "./conflictText";

/** Filter labels for the categories ForgeSync writes today; others show as-is. */
export const CATEGORIES: { key: string; label: string }[] = [
  { key: "session", label: "Sign-ins" },
  { key: "node", label: "Node health" },
  { key: "repo", label: "Repositories" },
  { key: "user", label: "Users" },
  { key: "conflict", label: "Conflicts" },
  { key: "inventory", label: "Scans" },
  { key: "history", label: "Exports" },
];

export function categoryLabel(key: string): string {
  return CATEGORIES.find((c) => c.key === key)?.label ?? key;
}

const str = (v: unknown) => (typeof v === "string" ? v : "");
const num = (v: unknown) => (typeof v === "number" ? String(v) : "?");
const refName = (v: unknown) => str(v).replace(/^refs\/(heads|tags)\//, "") || "a ref";

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
    case "repo.primary_assigned":
      return d.reason === "owner"
        ? `Primary set to ${str(d.to)}, the owner's primary site`
        : `Primary set to ${str(d.to)}, where the repository was created first`;
    case "user.home_assigned":
      return `Primary site set to ${str(d.to)}, where the user registered`;
    case "user.set_home": {
      const from = str(d.from) || "not set";
      return `Primary site changed from ${from} to ${str(d.to)}`;
    }
    case "repo.created_on_node":
      return `Created on ${str(d.node)}, copied from ${str(d.from || d.primary)}`;
    case "user.created_on_node":
      return `SceneID user created on ${str(d.node)} (as on ${str(d.from)}), linked on first sign-in`;
    case "repo.renamed_on_primary":
      return `Renamed on the primary ${str(d.primary)} from ${str(d.from)} to ${str(d.to)}`;
    case "repo.renamed_on_node":
      return `Copy on ${str(d.node)} renamed from ${str(d.from)}, as on the primary`;
    case "repo.deleted_on_primary":
      return `Deleted on the primary ${str(d.primary)}; archiving the other copies`;
    case "repo.archived_on_node":
      return `Copy on ${str(d.node)} archived as ${str(d.archived_as)}`;
    case "repo.archive_deleted":
      return `Archived copy ${str(d.archived_as)} on ${str(d.node)} deleted after the backup period`;
    case "repo.forgotten":
      return "Deleted repository forgotten: no copy is left";
    case "repo.recreated_on_primary":
      return `Created again on the primary ${str(d.primary)}`;
    case "repo.replicate_requested":
      return "Asked for replication";
    case "inventory.scan_requested":
      return "Asked for a repository scan";
    case "conflict.auto_fixed":
      return d.fix === "default_branch"
        ? `Default branch on ${str(d.node)} set to ${str(d.to)}, as on the primary`
        : `Took ${refName(d.ref)} from ${str(d.node)} over to the primary ${str(d.primary)}`;
    case "conflict.handed_off":
      return `Handed ${refName(d.ref)} to the owner: pull request #${num(d.pr_number)} on the primary`;
    case "conflict.owner_merged":
      return `Owner merged pull request #${num(d.pr_number)}: the other sites' commits on ${refName(d.ref)} are kept`;
    case "conflict.owner_kept_primary":
      return `Owner closed pull request #${num(d.pr_number)}: the primary's ${refName(d.ref)} is kept, with a backup on ${str(d.backup)}`;
    case "conflict.replica_reset":
      return `${refName(d.ref)} on ${str(d.node)} reset to the primary's version, as the owner chose`;
    case "conflict.backup_deleted":
      return `Backup branch ${str(d.branch)} deleted after its retention period`;
    case "conflict.handoff_gone":
      return `Pull request #${num(d.pr_number)} for ${refName(d.ref)} disappeared; it's handed off again if still needed`;
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
  const label = KIND_LABEL[kind as keyof typeof KIND_LABEL];
  return label ? label.toLowerCase() : kind;
}

/** Where the event's target lives in the UI, if anywhere. */
export function targetLink(e: HistoryEvent): string | undefined {
  const d = e.details ?? {};
  if (e.category === "node" && e.target) return `/nodes/${encodeURIComponent(e.target)}`;
  if (e.category === "conflict" && typeof d.conflict_id === "number") return `/conflicts/${d.conflict_id}`;
  if (e.category === "repo" && str(d.repository_id)) return `/repositories/${str(d.repository_id)}`;
  return undefined;
}
