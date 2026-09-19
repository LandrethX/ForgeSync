import type { ReactNode } from "react";
import { hasRole, type Role, type Session } from "../api";
import { useLeadership, type Connection } from "../hooks";
import { Link, usePath } from "../router";

const NAV: { to: string; label: string; role: Role }[] = [
  { to: "/", label: "Dashboard", role: "viewer" },
  { to: "/nodes", label: "Nodes", role: "viewer" },
  { to: "/repositories", label: "Repositories", role: "viewer" },
  { to: "/users", label: "Users", role: "viewer" },
  { to: "/conflicts", label: "Conflicts", role: "viewer" },
  { to: "/audit", label: "Events & audit", role: "operator" },
];

const ROLE_LABEL: Record<Role, string> = {
  viewer: "Viewer",
  operator: "Operator",
  administrator: "Administrator",
};

/**
 * A standby serves every page, so without this it looks exactly like the
 * controller doing the work and its buttons quietly turn away.
 */
function StandbyBanner() {
  const { readOnly, leaderName, leaderURL } = useLeadership();
  if (!readOnly) return null;
  const who = leaderName || "another controller";
  return (
    <div className="standby-banner" role="status">
      <strong>This controller is on standby.</strong> You can read everything
      here, but {who} is the one doing the work, and anything that changes the
      installation has to be done there.{" "}
      {leaderURL && (
        <a href={leaderURL} rel="noreferrer">
          Open {who}
        </a>
      )}
    </div>
  );
}

function isActive(to: string, path: string): boolean {
  return to === "/" ? path === "/" : path === to || path.startsWith(`${to}/`);
}

const CONNECTION_TEXT: Record<Connection, string> = {
  connecting: "Connecting…",
  live: "Live",
  reconnecting: "Reconnecting…",
};

export function Layout({
  children,
  connection,
  session,
  onSignOut,
}: {
  children: ReactNode;
  connection: Connection;
  session: Session;
  onSignOut: () => void;
}) {
  const path = usePath();
  return (
    <div className="shell">
      <a className="skip-link" href="#main">
        Skip to content
      </a>

      <header className="topbar">
        <Link to="/" className="brand">
          <img src="/favicon.svg" alt="" width={24} height={24} />
          ForgeSync
        </Link>
        <div className="topbar-right">
          <span
            className={`connection connection-${connection}`}
            role="status"
            aria-live="polite"
          >
            <span className="connection-dot" aria-hidden="true" />
            {CONNECTION_TEXT[connection]}
          </span>
          <span className="who" title={session.email || undefined}>
            <span className="who-name">
              {session.source === "sceneid"
                ? session.name || session.username
                : "Admin token"}
            </span>
            <span className="who-role">{ROLE_LABEL[session.role]}</span>
          </span>
          <button type="button" className="button-quiet" onClick={onSignOut}>
            Sign out
          </button>
        </div>
      </header>
      <StandbyBanner />
      <nav className="sidenav" aria-label="Main">
        <ul>
          {NAV.filter((item) => hasRole(session, item.role)).map((item) => (
            <li key={item.to}>
              <Link
                to={item.to}
                aria-current={isActive(item.to, path) ? "page" : undefined}
              >
                {item.label}
              </Link>
            </li>
          ))}
        </ul>
      </nav>
      <main id="main" className="content" tabIndex={-1}>
        {children}
      </main>
    </div>
  );
}

export function PageHeader({
  title,
  children,
}: {
  title: ReactNode;
  children?: ReactNode;
}) {
  return (
    <div className="page-header">
      <h1>{title}</h1>
      {children}
    </div>
  );
}

export function ErrorNote({ message }: { message: string }) {
  return (
    <p className="error-note" role="alert">
      {message}
    </p>
  );
}
