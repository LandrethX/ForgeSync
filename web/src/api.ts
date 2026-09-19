// Client for the controller's admin API. The browser authenticates with an
// HttpOnly session cookie; it never sees the admin token after signing in.

export type NodeState =
  "UNKNOWN" | "HEALTHY" | "DEGRADED" | "SUSPECT" | "UNREACHABLE" | "AUTH_ERROR";

export interface Node {
  name: string;
  url: string;
  site: string;
  state: NodeState;
  version?: string;
  last_checked: string;
  last_seen?: string;
  failing_since?: string;
  consecutive_failures: number;
  last_error?: string;
}

export interface Transition {
  id: number;
  node: string;
  from: NodeState;
  to: NodeState;
  at: string;
  error?: string;
}

export interface HistoryEvent {
  id: string;
  at: string;
  category: string;
  actor: string;
  action: string;
  target: string;
  details: Record<string, unknown>;
}

export interface HistoryFilter {
  categories: string[];
  actor: string;
  q: string;
  from?: string; // RFC 3339
  to?: string;
}

function historyParams(f: HistoryFilter): URLSearchParams {
  const p = new URLSearchParams();
  if (f.categories.length) p.set("category", f.categories.join(","));
  if (f.actor) p.set("actor", f.actor);
  if (f.q) p.set("q", f.q);
  if (f.from) p.set("from", f.from);
  if (f.to) p.set("to", f.to);
  return p;
}

/** The export is a plain download link; the session cookie authenticates it. */
export function historyExportURL(
  f: HistoryFilter,
  format: "csv" | "json",
): string {
  const p = historyParams(f);
  p.set("format", format);
  return `/api/v1/history/export?${p}`;
}

export type RepoStatus = "same" | "differs" | "missing" | "unknown" | "deleted";
export type Presence = "present" | "absent" | "unknown";

export interface Replica {
  node: string;
  present: boolean;
  forgejo_id: number;
  private: boolean;
  fork: boolean;
  mirror: boolean;
  archived: boolean;
  empty: boolean;
  default_branch: string;
  head_sha: string;
  head_error?: string;
  forgejo_updated_at?: string;
  last_seen_at?: string;
  /** The copy's name on this node; differs while a rename on the primary isn't applied here yet. */
  full_name?: string;
  checked_at: string;
}

export interface NodeView {
  node: string;
  presence: Presence;
  stale: boolean;
  replica?: Replica;
}

export type ReplicaState =
  "synced" | "conflict" | "error" | "waiting" | "missing" | "archived";

/** A copy of a repository deleted on its primary, kept on one node for a while. */
export interface Archive {
  id: number;
  node: string;
  original_name: string;
  archived_name: string;
  state: "renamed" | "archived" | "purged";
  archived_at: string;
  delete_after?: string;
}

export interface ReplicaSync {
  node: string;
  state: ReplicaState;
  detail?: string;
  last_attempt_at: string;
  last_success_at?: string;
  out_of_sync_since?: string;
  refs_updated: number;
}

export interface Repository {
  id: string;
  full_name: string;
  primary_node: string;
  /**
   * "owner": the owner's primary site; "origin": the node it was created on first (owners without a
   * primary site, e.g. organizations); "manual": an administrator chose it.
   */
  primary_source: "" | "owner" | "origin" | "manual";
  first_seen_at: string;
  status: RepoStatus;
  nodes: NodeView[];
  replication?: { enabled: boolean; replicas: ReplicaSync[] };
  /** When ForgeSync found it deleted on its primary. */
  deleted_at?: string;
  archives?: Archive[];
}

export interface UserAccount {
  node: string;
  login: string;
  present: boolean;
  forgejo_created_at?: string;
  created_by_forgesync: boolean;
}

export interface User {
  id: string;
  sub: string;
  login: string;
  home_node: string;
  /** "registration": where the account was created first; "manual": an administrator chose it. */
  home_source: "" | "registration" | "manual";
  first_seen_at: string;
  accounts: UserAccount[];
}

/** What creating an account did on each node. */
export interface UserWrite {
  login: string;
  created: string[];
  refused?: Record<string, string>;
}

export interface NodeWebhook {
  node: string;
  installed: boolean;
  hook_id?: number;
  error?: string;
  checked_at?: string;
  deliveries: number;
  rejected: number;
  last_delivery_at?: string;
  last_event?: string;
  last_repository?: string;
}

export interface WebhookStatus {
  enabled: boolean;
  nodes: NodeWebhook[];
}

export interface ReplicatedIssue {
  id: string;
  title: string;
  state: string;
  author: string;
  origin_node: string;
  created_at: string;
  deleted_at?: string;
  copies: Record<string, { number: number; forgejo_id: number }>;
  comments: number;
  numbers_differ: boolean;
}

export interface RepositoryList {
  total: number;
  counts: Partial<Record<RepoStatus, number>>;
  items: Repository[];
}

export interface NodeScan {
  node: string;
  started_at: string;
  finished_at: string;
  ok: boolean;
  error?: string;
  repositories: number;
  last_success_at?: string;
}

export interface InventoryStatus {
  running: boolean;
  interval_seconds: number;
  nodes: NodeScan[];
}

/** What one node has sent to another: the repositories it is the primary of. */
export interface SourcePair {
  from: string;
  to: string;
  repositories: number;
  in_sync: number;
  last_success_at?: string;
  last_attempt_at?: string;
}

export interface ReplicationSources {
  enabled: boolean;
  pairs: SourcePair[];
}

export type ConflictKind =
  | "git_diverged"
  | "default_branch_mismatch"
  | "git_replica_ahead"
  | "git_primary_rewrote"
  | "git_replica_changed"
  | "git_replica_extra_ref"
  | "issue_conflict"
  | "org_metadata"
  | "repo_metadata"
  | "actions_variable_conflict"
  | "actions_secret_missing"
  | "lfs_incomplete"
  | "package_incomplete"
  | "package_unreplicated";

export interface Handoff {
  pr_number: number;
  pr_url: string;
  branch: string;
  nodes: string[];
  state: string;
}

export interface Relation {
  a: string;
  b: string;
  relation: "diverged" | "a_behind_b" | "b_behind_a";
}

export interface Conflict {
  id: number;
  repository_id: string;
  full_name: string;
  primary_node: string;
  kind: ConflictKind;
  ref: string;
  state: "open" | "cleared";
  details: {
    branch?: string;
    tag?: string;
    heads?: Record<string, string>;
    branches?: Record<string, string>;
    relations?: Relation[];
    primary?: string;
    /** Pull requests on the primary where the owner decides (diverged branches). */
    handoffs?: Handoff[];
    /** issue_conflict: the field (title, body, state, labels, milestone, assignees, comment,
     * deleted, comment deleted), or "<label|milestone> <field>" for a label or milestone of
     * its own. */
    field?: string;
    /** issue_conflict: each node's value of the field. */
    values?: Record<string, string>;
    /** issue_conflict: the issue's number on each node. */
    issue?: Record<string, number>;
    /** issue_conflict (deleted): the node whose copy was changed. */
    node?: string;
    /** issue_conflict (label or milestone): its name on the nodes that agree. */
    item?: string;
    /** issue_conflict (assignees): "<node>: <why>" for each node that can't hold the value. */
    blocked?: string[];
    /** org_metadata: the organization whose profile differs. */
    organization?: string;
    /** Set when the difference is in the repository's wiki, not its own refs. */
    wiki?: boolean;
    /** actions_variable_conflict: the variable whose value differs; its values are in `values`. */
    variable?: string;
    /** actions_secret_missing: the secret, and the nodes that haven't got it. */
    secret?: string;
    missing?: string[];
    /** package_*: the package this is about, and what each node holds under a
     * contested file name. `node` is the node that's short of it, when it's one node. */
    owner?: string;
    package_type?: string;
    package?: string;
    version?: string;
    file?: string;
    digests?: Record<string, string>;
    reason?: string;
    /** lfs_incomplete: how many LFS objects the node is short of and some of them,
     * or why it couldn't be asked. The node is in `node`, as for a deleted issue. */
    objects?: number;
    oids?: string[];
    error?: string;
  };
  detected_at: string;
  last_seen_at: string;
  cleared_at?: string;
  acknowledged_by?: string;
  acknowledged_at?: string;
  note?: string;
}

export interface ConflictList {
  total: number;
  counts: { open?: number; cleared?: number };
  items: Conflict[];
}

export interface Overview {
  version: string;
  commit: string;
  started_at: string;
  /** single, leader or standby. */
  role: string;
  /** Who is acting, when two controllers share a database. */
  leader?: {
    name?: string;
    url?: string;
    since?: string;
    /** Why leadership is unknown, if the database can't be reached. */
    error?: string;
  };
  database: { ok: boolean; error?: string };
  nodes: Record<string, number>;
  open_conflicts: number;
  replication: {
    enabled: boolean;
    counts: Partial<Record<ReplicaState, number>>;
    /** What replication covers here, besides branches and tags. */
    features?: string[];
  };
  /** Every controller sharing this database, with the one that is acting. */
  controllers?: Controller[];
  /** The controller an administrator asked to lead, if anyone has. */
  chosen?: { controller: string; chosen_by?: string; chosen_at?: string };
}

/** One ForgeSync controller: where it is and what it's doing. */
export interface Controller {
  name: string;
  url?: string;
  /** Where the database sees it connect from. */
  address?: string;
  version?: string;
  /** "leader" does the work, "standby" is ready to, "unknown" has been quiet. */
  role: "leader" | "standby" | "unknown";
  /** True for the controller answering this request. */
  self: boolean;
  /** The controller meant to lead whenever it's running. */
  preferred?: boolean;
  /** The controller an administrator asked to lead. */
  chosen?: boolean;
  /** What was configured; lower leads, 0 means no preference. */
  priority?: number;
  started_at: string;
  last_seen_at: string;
}

export type Role = "viewer" | "operator" | "administrator";

/**
 * One of ForgeSync's own accounts: someone who signs in to the
 * controllers. They live in ForgeSync's database, which both controllers
 * share, and are never copied to a Forgejo node.
 */
export interface Account {
  id: string;
  username: string;
  full_name?: string;
  role: Role;
  disabled: boolean;
  created_at: string;
  created_by?: string;
  updated_at: string;
  last_sign_in?: string;
}

export interface Session {
  subject: string;
  username: string;
  name: string;
  email?: string;
  role: Role;
  source: "sceneid" | "account" | "token" | "web-token";
  expires_at: string;
}

export interface AuthConfig {
  sceneid: boolean;
  token_sign_in: boolean;
}

const ROLE_RANK: Record<Role, number> = {
  viewer: 1,
  operator: 2,
  administrator: 3,
};

export function hasRole(session: Session | undefined, min: Role): boolean {
  return session !== undefined && ROLE_RANK[session.role] >= ROLE_RANK[min];
}

/** Where the SceneID sign-in starts; the browser navigates there. */
export function sceneIdLoginURL(returnTo: string): string {
  return `/api/v1/auth/login?return_to=${encodeURIComponent(returnTo)}`;
}

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
    readonly retryAfterSeconds?: number,
  ) {
    super(message);
  }
}

/** Fired when any request finds the session gone, so the app can show sign-in. */
export const SIGNED_OUT_EVENT = "forgesync:signed-out";

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
): Promise<T> {
  const headers: Record<string, string> = {
    Accept: "application/json",
    // Required by the server for cookie-authenticated writes (CSRF defence).
    "X-ForgeSync-CSRF": "1",
  };
  if (body !== undefined) headers["Content-Type"] = "application/json";

  let res: Response;
  try {
    res = await fetch(`/api/v1${path}`, {
      method,
      headers,
      credentials: "same-origin",
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch {
    throw new ApiError(0, "Can't reach the ForgeSync controller.");
  }

  if (res.status === 204) return undefined as T;
  const data: unknown = await res.json().catch(() => ({}));
  if (!res.ok) {
    const message =
      typeof data === "object" &&
      data !== null &&
      "message" in data &&
      typeof data.message === "string"
        ? data.message
        : res.statusText;
    if (
      res.status === 401 &&
      path !== "/session" &&
      !path.startsWith("/auth/")
    ) {
      window.dispatchEvent(new Event(SIGNED_OUT_EVENT));
    }
    const retry = Number(res.headers.get("Retry-After"));
    throw new ApiError(
      res.status,
      message,
      Number.isFinite(retry) && retry > 0 ? retry : undefined,
    );
  }
  return data as T;
}

export const api = {
  authConfig: () => request<AuthConfig>("GET", "/auth/config"),
  session: () => request<Session>("GET", "/session"),
  signIn: (token: string) => request<Session>("POST", "/session", { token }),
  signOut: () => request<{ logout_url: string }>("DELETE", "/session"),
  overview: () => request<Overview>("GET", "/overview"),
  nodes: () => request<Node[]>("GET", "/nodes"),
  node: (name: string) =>
    request<Node>("GET", `/nodes/${encodeURIComponent(name)}`),
  transitions: (node: string | null, limit: number) =>
    request<Transition[]>(
      "GET",
      node
        ? `/nodes/${encodeURIComponent(node)}/transitions?limit=${limit}`
        : `/transitions?limit=${limit}`,
    ),
  repositories: (opts: {
    q?: string;
    status?: RepoStatus;
    limit: number;
    offset: number;
  }) => {
    const p = new URLSearchParams({
      limit: String(opts.limit),
      offset: String(opts.offset),
    });
    if (opts.q) p.set("q", opts.q);
    if (opts.status) p.set("status", opts.status);
    return request<RepositoryList>("GET", `/repositories?${p}`);
  },
  repository: (id: string) =>
    request<Repository>("GET", `/repositories/${encodeURIComponent(id)}`),
  repositoryIssues: (id: string) =>
    request<{ issues: ReplicatedIssue[] }>(
      "GET",
      `/repositories/${encodeURIComponent(id)}/issues`,
    ),
  setPrimary: (id: string, node: string) =>
    request<{ primary_node: string; previous: string }>(
      "PUT",
      `/repositories/${encodeURIComponent(id)}/primary`,
      {
        node,
      },
    ),
  inventory: () => request<InventoryStatus>("GET", "/inventory"),
  webhooks: () => request<WebhookStatus>("GET", "/webhooks"),
  replicationSources: () =>
    request<ReplicationSources>("GET", "/replication/sources"),
  addUser: (u: {
    login: string;
    subject: string;
    full_name?: string;
    email?: string;
    home: string;
  }) => request<UserWrite>("POST", "/users", u),
  signInWithPassword: (username: string, password: string) =>
    request<Session>("POST", "/session", { username, password }),
  accounts: () =>
    request<{ total: number; items: Account[] }>("GET", "/accounts"),
  addAccount: (a: {
    username: string;
    password: string;
    full_name?: string;
    role: Role;
  }) => request<Account>("POST", "/accounts", a),
  updateAccount: (
    id: string,
    changes: { full_name?: string; role?: Role; disabled?: boolean },
  ) => request<Account>("PUT", `/accounts/${encodeURIComponent(id)}`, changes),
  setAccountPassword: (id: string, password: string) =>
    request<{ message: string }>(
      "PUT",
      `/accounts/${encodeURIComponent(id)}/password`,
      { password },
    ),
  deleteAccount: (id: string) =>
    request<{ message: string }>(
      "DELETE",
      `/accounts/${encodeURIComponent(id)}`,
    ),
  chooseLeader: (controller: string) =>
    request<{ controller: string; message: string }>("PUT", "/leadership", {
      controller,
    }),
  clearLeaderChoice: () =>
    request<{ message: string }>("DELETE", "/leadership"),
  provisionUser: (id: string) =>
    request<UserWrite>("POST", `/users/${encodeURIComponent(id)}/nodes`),
  users: (q?: string) =>
    request<{ total: number; items: User[] }>(
      "GET",
      `/users${q ? `?q=${encodeURIComponent(q)}` : ""}`,
    ),
  setUserHome: (id: string, node: string) =>
    request<{ home_node: string; previous: string }>(
      "PUT",
      `/users/${encodeURIComponent(id)}/home`,
      { node },
    ),
  replicateNow: (id: string) =>
    request<{ queued: boolean; running: boolean }>(
      "POST",
      `/repositories/${encodeURIComponent(id)}/replicate`,
    ),
  scanNow: () =>
    request<{ queued: boolean; running: boolean }>("POST", "/inventory/scan"),
  conflicts: (opts: {
    state: "open" | "cleared" | "all";
    repository?: string;
    limit: number;
    offset: number;
  }) => {
    const p = new URLSearchParams({
      state: opts.state,
      limit: String(opts.limit),
      offset: String(opts.offset),
    });
    if (opts.repository) p.set("repository", opts.repository);
    return request<ConflictList>("GET", `/conflicts?${p}`);
  },
  conflict: (id: number) => request<Conflict>("GET", `/conflicts/${id}`),
  acknowledgeConflict: (id: number, note: string) =>
    request<Conflict>("POST", `/conflicts/${id}/acknowledge`, { note }),
  history: (f: HistoryFilter, limit: number, cursor?: string) => {
    const p = historyParams(f);
    p.set("limit", String(limit));
    if (cursor) p.set("cursor", cursor);
    return request<{ items: HistoryEvent[]; next_cursor?: string }>(
      "GET",
      `/history?${p}`,
    );
  },
  historyActors: () => request<string[]>("GET", "/history/actors"),
};
