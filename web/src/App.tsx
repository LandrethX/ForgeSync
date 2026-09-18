import { useCallback, useEffect, useState } from "react";
import { api, ApiError, SIGNED_OUT_EVENT } from "./api";
import { Layout, PageHeader } from "./components/Layout";
import { NodeStreamContext, useNodeStream } from "./hooks";
import { Audit } from "./pages/Audit";
import { Dashboard } from "./pages/Dashboard";
import { Login } from "./pages/Login";
import { NodeDetail } from "./pages/NodeDetail";
import { Nodes } from "./pages/Nodes";
import { Link, match, usePath } from "./router";

type Auth = "checking" | "signed-out" | "signed-in" | "unavailable";

export function App() {
  const [auth, setAuth] = useState<Auth>("checking");

  const check = useCallback(() => {
    api
      .session()
      .then(() => setAuth("signed-in"))
      .catch((e: unknown) => setAuth(e instanceof ApiError && e.status === 401 ? "signed-out" : "unavailable"));
  }, []);

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
      return <Login onSignedIn={() => setAuth("signed-in")} />;
    case "signed-in":
      return <SignedIn onSignOut={() => api.signOut().finally(() => setAuth("signed-out"))} />;
  }
}

function SignedIn({ onSignOut }: { onSignOut: () => void }) {
  const stream = useNodeStream();
  return (
    <NodeStreamContext.Provider value={stream}>
      <Layout connection={stream.connection} onSignOut={onSignOut}>
        <Routes />
      </Layout>
    </NodeStreamContext.Provider>
  );
}

function Routes() {
  const path = usePath();
  useEffect(() => {
    document.getElementById("main")?.focus({ preventScroll: true });
    window.scrollTo(0, 0);
  }, [path]);

  if (match("/", path)) return <Dashboard />;
  if (match("/nodes", path)) return <Nodes />;
  const node = match("/nodes/:name", path);
  if (node?.name) return <NodeDetail name={node.name} />;
  if (match("/audit", path)) return <Audit />;
  return (
    <>
      <PageHeader title="Page not found" />
      <p>
        <Link to="/">Go to the dashboard</Link>
      </p>
    </>
  );
}
