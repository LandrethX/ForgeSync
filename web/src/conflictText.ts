import type { Conflict, Relation } from "./api";

export const KIND_LABEL: Record<Conflict["kind"], string> = {
  git_diverged: "Diverged history",
  default_branch_mismatch: "Different default branches",
};

/** One-line summary, e.g. "Diverged history on main". */
export function conflictTitle(c: Conflict): string {
  if (c.kind === "git_diverged") return `${KIND_LABEL.git_diverged} on ${c.details.branch ?? c.ref.replace(/^refs\/heads\//, "")}`;
  return KIND_LABEL[c.kind] ?? c.kind;
}

export function relationText(r: Relation): string {
  switch (r.relation) {
    case "diverged":
      return `${r.a} and ${r.b} have diverged: each has commits the other doesn't.`;
    case "a_behind_b":
      return `${r.a} is behind ${r.b} and could be fast-forwarded.`;
    case "b_behind_a":
      return `${r.b} is behind ${r.a} and could be fast-forwarded.`;
  }
}

/** Node name -> what it has (commit or branch), sorted by node. */
export function conflictSides(c: Conflict): [string, string][] {
  const m = (c.kind === "default_branch_mismatch" ? c.details.branches : c.details.heads) ?? {};
  return Object.entries(m).sort(([a], [b]) => a.localeCompare(b));
}
