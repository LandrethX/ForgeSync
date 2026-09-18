// Client for the controller's admin API. The browser authenticates with an
// HttpOnly session cookie; it never sees the admin token after signing in.

export type NodeState = "UNKNOWN" | "HEALTHY" | "DEGRADED" | "SUSPECT" | "UNREACHABLE" | "AUTH_ERROR";

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

export interface AuditEntry {
  id: number;
  at: string;
  actor: string;
  action: string;
  target: string;
  details: Record<string, unknown>;
}

export interface Overview {
  version: string;
  commit: string;
  started_at: string;
  role: string;
  database: { ok: boolean; error?: string };
  nodes: Record<string, number>;
}

export interface Session {
  subject: string;
  expires_at: string;
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

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
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
      typeof data === "object" && data !== null && "message" in data && typeof data.message === "string"
        ? data.message
        : res.statusText;
    if (res.status === 401 && path !== "/session") {
      window.dispatchEvent(new Event(SIGNED_OUT_EVENT));
    }
    const retry = Number(res.headers.get("Retry-After"));
    throw new ApiError(res.status, message, Number.isFinite(retry) && retry > 0 ? retry : undefined);
  }
  return data as T;
}

export const api = {
  session: () => request<Session>("GET", "/session"),
  signIn: (token: string) => request<Session>("POST", "/session", { token }),
  signOut: () => request<void>("DELETE", "/session"),
  overview: () => request<Overview>("GET", "/overview"),
  nodes: () => request<Node[]>("GET", "/nodes"),
  node: (name: string) => request<Node>("GET", `/nodes/${encodeURIComponent(name)}`),
  transitions: (node: string | null, limit: number) =>
    request<Transition[]>(
      "GET",
      node ? `/nodes/${encodeURIComponent(node)}/transitions?limit=${limit}` : `/transitions?limit=${limit}`,
    ),
  audit: (limit: number, before?: number) =>
    request<AuditEntry[]>("GET", `/audit?limit=${limit}${before ? `&before=${before}` : ""}`),
};
