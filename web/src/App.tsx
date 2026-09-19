import { useCallback, useEffect, useState } from "react";
import { api, ApiError, hasRole, SIGNED_OUT_EVENT, type Session } from "./api";
import { Layout, PageHeader } from "./components/Layout";
import {
  LeadershipContext,
  NodeStreamContext,
  SessionContext,
  useLeadershipPoll,
  useNodeStream,
} from "./hooks";
import { ConflictDetail } from "./pages/ConflictDetail";
import { Conflicts } from "./pages/Conflicts";
import { Dashboard } from "./pages/Dashboard";
import { History } from "./pages/History";
import { Login } from "./pages/Login";
import { NodeDetail } from "./pages/NodeDetail";
import { Nodes } from "./pages/Nodes";
import { Repositories } from "./pages/Repositories";
import { Accounts } from "./pages/Accounts";
import { Users } from "./pages/Users";
import { RepositoryDetail } from "./pages/RepositoryDetail";
import { Link, match, usePath } from "./router";

type Auth = "checking" | "signed-out" | "signed-in" | "unavailable";

export function App() {
  const [auth, setAuth] = useState<Auth>("checking");
  const [session, setSession] = useState<Session>();

  const check = useCallback(() => {
    api
      .session()
      .then((s) => {
        setSession(s);
        setAuth("signed-in");
      })
      .catch((e: unknown) =>
        setAuth(
          e instanceof ApiError && e.status === 401
            ? "signed-out"
            : "unavailable",
        ),
      );
  }, []);

  const signedIn = (s: Session) => {
    setSession(s);
    setAuth("signed-in");
  };

  const signOut = () => {
    api
      .signOut()
      .then((r) => {
        // SceneID sessions also end at SceneID, which then sends the browser back here.
        if (r?.logout_url) window.location.assign(r.logout_url);
        else setAuth("signed-out");
      })
      .catch(() => setAuth("signed-out"));
  };

  useEffect(() => {
    check();
    const onSignedOut = () => setAuth("signed-out");
    window.addEventListener(SIGNED_OUT_EVENT, onSignedOut);
    return () => window.removeEventListener(SIGNED_OUT_EVENT, onSignedOut);
  }, [check]);

  switch (auth) {
    case "checking":
      return <p className="center muted">Loading…</p>;
    case "unavailable":
      return (
        <div className="center">
          <p>Can't reach the ForgeSync controller.</p>
          <button type="button" className="button-primary" onClick={check}>
            Try again
          </button>
        </div>
      );
    case "signed-out":
      return <Login onSignedIn={signedIn} />;
    case "signed-in":
      return session ? (
        <SignedIn session={session} onSignOut={signOut} />
      ) : null;
  }
}

function SignedIn({
  session,
  onSignOut,
}: {
  session: Session;
  onSignOut: () => void;
}) {
  const stream = useNodeStream();
  const leadership = useLeadershipPoll();
  return (
    <SessionContext.Provider value={session}>
      <LeadershipContext.Provider value={leadership}>
        <NodeStreamContext.Provider value={stream}>
          <Layout
            connection={stream.connection}
            session={session}
            onSignOut={onSignOut}
          >
            <Routes session={session} />
          </Layout>
        </NodeStreamContext.Provider>
      </LeadershipContext.Provider>
    </SessionContext.Provider>
  );
}

function Routes({ session }: { session: Session }) {
  const path = usePath();
  useEffect(() => {
    document.getElementById("main")?.focus({ preventScroll: true });
    window.scrollTo(0, 0);
  }, [path]);

  if (match("/", path)) return <Dashboard />;
  if (match("/nodes", path)) return <Nodes />;
  const node = match("/nodes/:name", path);
  if (node?.name) return <NodeDetail name={node.name} />;
  if (match("/repositories", path)) return <Repositories />;
  const repo = match("/repositories/:id", path);
  if (repo?.id) return <RepositoryDetail id={repo.id} />;
  if (match("/users", path)) return <Users />;
  if (match("/accounts", path)) {
    return hasRole(session, "administrator") ? (
      <Accounts />
    ) : (
      <>
        <PageHeader title="ForgeSync accounts" />
        <p>
          Managing who can sign in to ForgeSync needs the administrator role.
        </p>
      </>
    );
  }
  if (match("/conflicts", path)) return <Conflicts />;
  const conflict = match("/conflicts/:id", path);
  if (conflict?.id && /^\d+$/.test(conflict.id))
    return <ConflictDetail id={Number(conflict.id)} />;
  if (match("/audit", path)) {
    return hasRole(session, "operator") ? (
      <History />
    ) : (
      <>
        <PageHeader title="Events & audit" />
        <p>
          The event and audit history needs the operator role. Ask an
          administrator if you need it.
        </p>
      </>
    );
  }
  return (
    <>
      <PageHeader title="Page not found" />
      <p>
        <Link to="/">Go to the dashboard</Link>
      </p>
    </>
  );
}
