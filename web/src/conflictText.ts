import type { Conflict, Relation } from "./api";

export const KIND_LABEL: Record<Conflict["kind"], string> = {
  git_diverged: "Diverged history",
  default_branch_mismatch: "Different default branches",
  git_replica_ahead: "Replica has its own commits",
  git_primary_rewrote: "History rewritten on the primary",
  git_replica_changed: "Changed on the replica",
  git_replica_extra_ref: "Only on the replica",
};

function refName(c: Conflict): string {
  return c.details.branch ?? c.details.tag ?? c.ref.replace(/^refs\/(heads|tags)\//, "");
}

/** One-line summary, e.g. "Diverged history on main". */
export function conflictTitle(c: Conflict): string {
  const label = KIND_LABEL[c.kind] ?? c.kind;
  if (c.kind === "default_branch_mismatch") return label;
  const what = c.details.tag !== undefined || c.ref.startsWith("refs/tags/") ? `tag ${refName(c)}` : refName(c);
  return `${label}: ${what}`;
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

/** What happened, in a sentence, for replication conflicts. */
export function conflictExplanation(c: Conflict): string | undefined {
  const primary = c.details.primary || c.primary_node || "the primary";
  switch (c.kind) {
    case "git_replica_ahead":
      return `Someone pushed commits to a replica that ${primary} doesn't have. Replication won't overwrite them.`;
    case "git_primary_rewrote":
      return `The history on ${primary} was rewritten (a force-push or a moved tag) since it was last replicated. Copying it would discard what the replicas have.`;
    case "git_replica_changed":
      return "This ref was changed on a replica after replication wrote it, or a tag there points somewhere else.";
    case "git_replica_extra_ref":
      return `This ref exists only on a replica; it wasn't created on ${primary}.`;
  }
  return undefined;
}

/** Guidance for fixing each kind of conflict. */
export function conflictFix(c: Conflict): string {
  const primary = c.details.primary || c.primary_node;
  const onPrimary = primary ? `on the primary (${primary})` : "on the side you trust";
  switch (c.kind) {
    case "git_replica_ahead":
      return `ForgeSync normally takes a replica's new commits over to ${primary || "the primary"} by itself. It couldn't here, most likely because the primary changed at the same time; the next run tries again.`;
    case "git_primary_rewrote":
      return "If the rewrite was intended, reset the branch or tag on each replica to the primary's value; replication won't do this itself. Otherwise restore the old value on the primary.";
    case "git_replica_changed":
      return `Decide which value is right. Set it ${onPrimary} and make the replica match, or delete the ref on the replica so replication recreates it from the primary.`;
    case "git_replica_extra_ref":
      return `ForgeSync normally creates a ref like this on ${primary || "the primary"} by itself, so it replicates from there. It couldn't here, most likely because the same name appeared on the primary in the meantime; the next run tries again. If the ref shouldn't exist, delete it on the replica.`;
    case "default_branch_mismatch":
      return "ForgeSync sets each replica's default branch to the primary's once that branch exists there. If this stays, set it in the repository settings on the nodes that differ.";
    default:
      return `ForgeSync never overwrites diverged history. Someone who knows the repository has to reconcile it ${onPrimary}: either merge the replica's commits in (a merge keeps them, so replication can then fast-forward the replica), or bring their changes over some other way (rebase, cherry-pick) and then reset the branch on the replica to the primary's commit, since those create new commits and the replica's originals stay diverged.`;
  }
}
