import type { Conflict, Relation } from "./api";

export const KIND_LABEL: Record<Conflict["kind"], string> = {
  git_diverged: "Diverged history",
  default_branch_mismatch: "Different default branches",
  git_replica_ahead: "Replica has its own commits",
  git_primary_rewrote: "History rewritten on the primary",
  git_replica_changed: "Changed on the replica",
  git_replica_extra_ref: "Only on the replica",
  issue_conflict: "Issue changed differently",
  org_metadata: "Organization changed differently",
  repo_metadata: "Setting changed differently",
  actions_variable_conflict: "Variable changed differently",
  actions_secret_missing: "Secret missing on a node",
  lfs_incomplete: "LFS objects missing on a node",
};

/**
 * For a conflict about a label or milestone of its own, its kind and the
 * field that differs; issue_conflict carries those as "<kind> <field>".
 */
function itemField(
  c: Conflict,
): { kind: "label" | "milestone"; field: string } | undefined {
  if (c.kind !== "issue_conflict") return undefined;
  const f = c.details.field ?? "";
  for (const kind of ["label", "milestone"] as const) {
    if (f.startsWith(`${kind} `))
      return { kind, field: f.slice(kind.length + 1) };
  }
  return undefined;
}

/** "the label bug", or just "the label" if its name is unknown. */
function itemName(c: Conflict, kind: string): string {
  return c.details.item ? `the ${kind} ${c.details.item}` : `the ${kind}`;
}

/** "dk", "dk and de", "dk, de and uk". */
function nodeList(names: string[]): string {
  if (names.length <= 1) return names[0] ?? "";
  return `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}`;
}

/** A wiki's branch is written wiki:refs/heads/main, so it can't be taken
 *  for the repository's own. */
function inWiki(c: Conflict): boolean {
  return c.details.wiki === true || c.ref.startsWith("wiki:");
}

function refName(c: Conflict): string {
  return (
    c.details.branch ??
    c.details.tag ??
    c.ref.replace(/^wiki:/, "").replace(/^refs\/(heads|tags)\//, "")
  );
}

/** One-line summary, e.g. "Diverged history on main". */
export function conflictTitle(c: Conflict): string {
  const label = KIND_LABEL[c.kind] ?? c.kind;
  if (c.kind === "default_branch_mismatch") return label;
  if (c.kind === "actions_variable_conflict") {
    return `${label}: ${c.details.variable ?? c.ref}`;
  }
  if (c.kind === "actions_secret_missing") {
    const where = c.details.missing?.join(", ");
    const secret = c.details.secret ?? c.ref;
    return where
      ? `Secret ${secret} is missing on ${where}`
      : `${label}: ${secret}`;
  }
  if (c.kind === "lfs_incomplete") {
    const node = c.details.node ?? c.ref;
    return c.details.objects
      ? `${c.details.objects} LFS object${c.details.objects === 1 ? "" : "s"} missing on ${node}`
      : `LFS objects can't be checked on ${node}`;
  }
  if (c.kind === "repo_metadata") {
    return `${label}: ${c.details.field ?? c.ref}`;
  }
  if (c.kind === "org_metadata") {
    return `${label}: ${c.details.organization ?? ""} ${c.details.field ?? ""}`.trimEnd();
  }
  if (c.kind === "issue_conflict") {
    const item = itemField(c);
    if (item) {
      const what = item.kind === "label" ? "Label" : "Milestone";
      return `${what} changed differently: ${c.details.item ?? c.ref}`;
    }
    return `${label}: ${c.ref}`;
  }
  const what =
    c.details.tag !== undefined || c.ref.startsWith("refs/tags/")
      ? `tag ${refName(c)}`
      : refName(c);
  if (inWiki(c)) return `${label}: ${what} in the wiki`;
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
  const m =
    (c.kind === "default_branch_mismatch"
      ? c.details.branches
      : c.kind === "issue_conflict" || c.kind === "actions_variable_conflict"
        ? c.details.values
        : c.details.heads) ?? {};
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
    case "actions_variable_conflict":
      return `The Actions variable ${c.details.variable ?? c.ref} was set to different values on different nodes since they last agreed. ForgeSync doesn't pick one, so none of them was changed.`;
    case "actions_secret_missing": {
      const missing = c.details.missing ?? [];
      const who = missing.length > 0 ? nodeList(missing) : "A node";
      const has = missing.length > 1 ? "haven't" : "hasn't";
      return `${who} ${has} got the Actions secret ${c.details.secret ?? c.ref}, which the other nodes have. Forgejo never gives a secret's value back, so ForgeSync can't copy one; a workflow that needs it would fail there.`;
    }
    case "lfs_incomplete": {
      const node = c.details.node ?? c.ref;
      if (c.details.error) {
        return `ForgeSync couldn't ask ${node} which LFS objects it has: ${c.details.error}. A repository with LFS files can't be checked out there until this works.`;
      }
      return `${node} is missing ${c.details.objects ?? "some"} of the LFS objects the other nodes have. The pointer files are in git, so a clone from ${node} would fail to fetch those files. ForgeSync copies objects by itself, so something is refusing them.`;
    }
    case "repo_metadata":
      return `The repository's ${c.details.field ?? "setting"} was set differently on different nodes since they last agreed. ForgeSync doesn't pick one, so none of them was changed.`;
    case "org_metadata":
      return `The ${c.details.field ?? "profile"} of the organization ${c.details.organization ?? ""} was set differently on different nodes since they last agreed. ForgeSync doesn't pick one, so none of them was changed.`;
    case "issue_conflict": {
      const item = itemField(c);
      if (item) {
        const what = itemName(c, item.kind);
        if (item.field === "deleted") {
          const node = c.details.node ?? "another node";
          return `${what.charAt(0).toUpperCase()}${what.slice(1)} was deleted on ${primary}, but changed on ${node} since. ForgeSync kept that copy, left it on the issues that have it there, and won't copy it back.`;
        }
        return `The ${item.field} of ${what} was changed to different values on different nodes since they last agreed. ForgeSync doesn't pick one.`;
      }
      if (c.details.blocked && c.details.blocked.length > 0) {
        return `ForgeSync can't give every node the same assignees (${c.details.blocked.join("; ")}), so it has left them all as they are.`;
      }
      switch (c.details.field) {
        case "deleted":
          return `The issue was deleted on ${primary}, but changed on ${c.details.node ?? "another node"} since. ForgeSync kept that copy and won't copy it back.`;
        case "comment deleted":
          return `The comment was deleted on ${primary}, but edited on ${c.details.node ?? "another node"} since. ForgeSync kept that copy.`;
        case "labels":
          return "The issue's labels were set differently on different nodes since they last agreed. ForgeSync doesn't pick one.";
        case "milestone":
          return "The issue was put in different milestones on different nodes since they last agreed. ForgeSync doesn't pick one.";
        case "assignees":
          return "The issue was assigned to different people on different nodes since they last agreed. ForgeSync doesn't pick one.";
        default:
          return `The ${c.details.field ?? "issue"} was changed to different values on different nodes since they last agreed. ForgeSync doesn't pick one.`;
      }
    }
  }
  return undefined;
}

/** Guidance for fixing each kind of conflict. */
export function conflictFix(c: Conflict): string {
  const primary = c.details.primary || c.primary_node;
  const onPrimary = primary
    ? `on the primary (${primary})`
    : "on the side you trust";
  switch (c.kind) {
    case "git_replica_ahead":
      return `ForgeSync normally takes a replica's new commits over to ${primary || "the primary"} by itself. It couldn't here, most likely because the primary changed at the same time; the next run tries again.`;
    case "git_primary_rewrote":
      return "If the rewrite was intended, reset the branch or tag on each replica to the primary's value; replication won't do this itself. Otherwise restore the old value on the primary.";
    case "git_replica_changed":
      return `Decide which value is right. Set it ${onPrimary} and make the replica match, or delete the ref on the replica so replication recreates it from the primary.`;
    case "git_replica_extra_ref":
      return `ForgeSync normally creates a ref like this on ${primary || "the primary"} by itself, so it replicates from there. It couldn't here, most likely because the same name appeared on the primary in the meantime; the next run tries again. If the ref shouldn't exist, delete it on the replica.`;
    case "issue_conflict": {
      const item = itemField(c);
      if (item) {
        if (item.field === "deleted") {
          return `Delete ${itemName(c, item.kind)} on ${c.details.node ?? "that node"} too if it should go. To keep it, create it again on ${primary || "the primary"} with the same ${item.kind === "label" ? "name" : "title"}; ForgeSync then treats it as normal again.`;
        }
        return `Set the ${item.field} of ${itemName(c, item.kind)} on one of the nodes so it matches another; ForgeSync then copies that value everywhere.`;
      }
      if (c.details.blocked && c.details.blocked.length > 0) {
        return "Forgejo only assigns people who can write to the repository, and ForgeSync doesn't replicate collaborators yet. Give them that access on the node that's named, or take them off the issue; ForgeSync then copies the assignees everywhere.";
      }
      if (
        c.details.field === "deleted" ||
        c.details.field === "comment deleted"
      ) {
        return `Delete it on ${c.details.node ?? "that node"} too if it should go. To keep it, create it again on ${primary} with the same author and title (or text); ForgeSync then treats it as normal again.`;
      }
      if (
        c.details.field === "labels" ||
        c.details.field === "milestone" ||
        c.details.field === "assignees"
      ) {
        return `Set the issue's ${c.details.field} on one of the nodes so it matches another; ForgeSync then copies that everywhere.`;
      }
      return `Edit the ${c.details.field ?? "field"} on one of the nodes so it matches another; ForgeSync then copies that value everywhere.`;
    }
    case "actions_variable_conflict":
      return `Set the variable ${c.details.variable ?? c.ref} on one of the nodes so it matches another; ForgeSync then copies that value everywhere.`;
    case "actions_secret_missing":
      return `Set the secret ${c.details.secret ?? c.ref} in the repository's Actions settings on ${nodeList(c.details.missing ?? []) || "the node that hasn't got it"}, with the same value as on the other nodes. Only someone who knows the value can do this.`;
    case "lfs_incomplete":
      return `Check that LFS is turned on for ${c.details.node ?? c.ref} ([server] LFS_START_SERVER) and that the repository's own LFS setting is on there. ForgeSync tries again every run; nothing is ever deleted, so a fixed node fills in by itself.`;
    case "repo_metadata":
      return `Set the ${c.details.field ?? "setting"} on one of the nodes so it matches another; ForgeSync then copies that everywhere. The default branch isn't settled here -- replication follows the primary's -- and a repository private on any node is made private on all of them.`;
    case "org_metadata":
      return `Set the ${c.details.field ?? "field"} on one of the nodes so it matches another; ForgeSync then copies that everywhere. An organization's profile has no primary: whichever value someone settles on wins.`;
    case "default_branch_mismatch":
      return "ForgeSync sets each replica's default branch to the primary's once that branch exists there. If this stays, set it in the repository settings on the nodes that differ.";
    default:
      return `ForgeSync never overwrites diverged history. Someone who knows the repository has to reconcile it ${onPrimary}: either merge the replica's commits in (a merge keeps them, so replication can then fast-forward the replica), or bring their changes over some other way (rebase, cherry-pick) and then reset the branch on the replica to the primary's commit, since those create new commits and the replica's originals stay diverged.`;
  }
}
