import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
} from "react";
import {
  api,
  ApiError,
  SIGNED_OUT_EVENT,
  type Node,
  type Session,
} from "./api";

/** Current time, re-rendering every `intervalMs` so relative times stay fresh. */
export function useNow(intervalMs = 1000): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), intervalMs);
    return () => window.clearInterval(id);
  }, [intervalMs]);
  return now;
}

export interface Loaded<T> {
  data: T | undefined;
  error: string | undefined;
  loading: boolean;
  reload: () => void;
}

/** Runs `load` on mount and whenever `key` changes; keeps the last data while reloading. */
export function useLoad<T>(
  load: () => Promise<T>,
  key: unknown[] = [],
): Loaded<T> {
  const [data, setData] = useState<T>();
  const [error, setError] = useState<string>();
  const [loading, setLoading] = useState(true);
  const [tick, setTick] = useState(0);
  const loadRef = useRef(load);
  loadRef.current = load;

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    loadRef
      .current()
      .then((d) => {
        if (!cancelled) {
          setData(d);
          setError(undefined);
        }
      })
      .catch((e: unknown) => {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [tick, ...key]);

  const reload = useCallback(() => setTick((t) => t + 1), []);
  return { data, error, loading, reload };
}

export type Connection = "connecting" | "live" | "reconnecting";

export interface NodeStream {
  nodes: Node[] | undefined;
  connection: Connection;
}

export const NodeStreamContext = createContext<NodeStream>({
  nodes: undefined,
  connection: "connecting",
});

export function useNodes(): NodeStream {
  return useContext(NodeStreamContext);
}

/**
 * Subscribes to the controller's server-sent events. The server pushes the
 * full node list after every health check; EventSource reconnects by itself.
 */
export function useNodeStream(): NodeStream {
  const [nodes, setNodes] = useState<Node[]>();
  const [connection, setConnection] = useState<Connection>("connecting");

  useEffect(() => {
    const es = new EventSource("/api/v1/events");
    es.addEventListener("open", () => setConnection("live"));
    es.addEventListener("nodes", (e) => {
      setConnection("live");
      setNodes(JSON.parse((e as MessageEvent<string>).data) as Node[]);
    });
    es.addEventListener("signed-out", () => {
      es.close();
      window.dispatchEvent(new Event(SIGNED_OUT_EVENT));
    });
    es.addEventListener("error", () => {
      setConnection("reconnecting");
      // A 401 looks like any other stream error; ask whether the session is
      // still there.
      api.session().catch((err: unknown) => {
        if (err instanceof ApiError && err.status === 401) {
          es.close();
          window.dispatchEvent(new Event(SIGNED_OUT_EVENT));
        }
      });
    });
    return () => es.close();
  }, []);

  return { nodes, connection };
}

/** Which controller this is, and who is in charge if it isn't this one. */
export type Leadership = {
  /** single, leader or standby; undefined until it's known. */
  role?: string;
  leaderName?: string;
  leaderURL?: string;
  /** True once it's known that this controller can't be written to. */
  readOnly: boolean;
  /** What replication covers here, from the controller's own config. */
  features?: string[];
};

export const LeadershipContext = createContext<Leadership>({ readOnly: false });

/**
 * A standby serves every page but turns away anything that changes the
 * installation, so the whole app needs to know which one it is talking to.
 */
export function useLeadership(): Leadership {
  return useContext(LeadershipContext);
}

/** Asks the controller which it is, and keeps asking: leadership moves. */
export function useLeadershipPoll(): Leadership {
  const [state, setState] = useState<Leadership>({ readOnly: false });
  useEffect(() => {
    let live = true;
    const refresh = () =>
      api
        .overview()
        .then((o) => {
          if (!live) return;
          setState({
            role: o.role,
            leaderName: o.leader?.name,
            leaderURL: o.leader?.url,
            readOnly: o.role === "standby",
            features: o.replication?.features,
          });
        })
        .catch(() => {
          // Leave it as it was: a hiccup shouldn't make the app look broken.
        });
    refresh();
    const id = window.setInterval(refresh, 15000);
    return () => {
      live = false;
      window.clearInterval(id);
    };
  }, []);
  return state;
}

export const SessionContext = createContext<Session | undefined>(undefined);

/** The signed-in user; only use inside the signed-in part of the app. */
export function useSession(): Session | undefined {
  return useContext(SessionContext);
}

/** `value`, but only after it has stopped changing for `ms`. */
export function useDebounced<T>(value: T, ms: number): T {
  const [v, setV] = useState(value);
  useEffect(() => {
    const id = window.setTimeout(() => setV(value), ms);
    return () => window.clearTimeout(id);
  }, [value, ms]);
  return v;
}
