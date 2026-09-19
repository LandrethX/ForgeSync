import { useState, type FormEvent } from "react";
import { ApiError, api, hasRole, type Account, type Role } from "../api";
import { ErrorNote, PageHeader } from "../components/Layout";
import { formatAgo, formatDateTime } from "../format";
import { useLoad, useNow, useSession } from "../hooks";

const ROLES: Role[] = ["viewer", "operator", "administrator"];

/**
 * ForgeSync's own accounts: the people who sign in to the controllers
 * themselves. They live in ForgeSync's database, which both controllers
 * share, so an account works on either one and keeps working when SceneID
 * is the thing that's unreachable. Nothing copies them to a Forgejo node.
 */
export function Accounts() {
  const session = useSession();
  const now = useNow();
  const list = useLoad(() => api.accounts(), []);
  const [error, setError] = useState<string>();

  if (!hasRole(session, "administrator")) {
    return (
      <>
        <PageHeader title="ForgeSync accounts" />
        <p>Managing accounts needs the administrator role.</p>
      </>
    );
  }

  return (
    <>
      <PageHeader title="ForgeSync accounts" />
      <p className="muted page-intro">
        Who can sign in to ForgeSync itself. These accounts are
        ForgeSync&rsquo;s own: they live in its database, which both controllers
        share, so they work on either one &mdash; including when SceneID
        can&rsquo;t be reached, which is when you most need them. They are never
        copied to a Forgejo node, and nobody gets an account there from having
        one here.
      </p>
      <AddAccount onAdded={list.reload} />
      {error && <ErrorNote message={error} />}
      {list.error && <ErrorNote message={list.error} />}
      {list.data && list.data.items.length === 0 && (
        <p className="muted">
          No accounts yet. Until there is one, sign in with the admin token.
        </p>
      )}
      {list.data && list.data.items.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">Account</th>
                <th scope="col">Role</th>
                <th scope="col">Can sign in</th>
                <th scope="col">Last signed in</th>
                <th scope="col">Added</th>
                <th scope="col">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {list.data.items.map((a) => (
                <AccountRow
                  key={a.id}
                  account={a}
                  now={now}
                  self={
                    session?.source === "account" && session.subject === a.id
                  }
                  onChanged={list.reload}
                  onError={setError}
                />
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

function AccountRow({
  account,
  now,
  self,
  onChanged,
  onError,
}: {
  account: Account;
  now: number;
  self: boolean;
  onChanged: () => void;
  onError: (message: string) => void;
}) {
  const [busy, setBusy] = useState(false);
  const [password, setPassword] = useState("");
  const [changing, setChanging] = useState(false);

  async function run(what: () => Promise<unknown>) {
    setBusy(true);
    try {
      await what();
      onChanged();
    } catch (e) {
      onError(e instanceof ApiError ? e.message : "That didn't work.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <tr>
      <th scope="row">
        {account.username}
        {account.full_name && (
          <div className="small muted">{account.full_name}</div>
        )}
        {self && <div className="small muted">This is you</div>}
      </th>
      <td>
        <label className="sr-only" htmlFor={`role-${account.id}`}>
          Role for {account.username}
        </label>
        <select
          id={`role-${account.id}`}
          value={account.role}
          disabled={busy}
          onChange={(e) =>
            run(() =>
              api.updateAccount(account.id, { role: e.target.value as Role }),
            )
          }
        >
          {ROLES.map((r) => (
            <option key={r} value={r}>
              {r}
            </option>
          ))}
        </select>
      </td>
      <td>{account.disabled ? "No" : "Yes"}</td>
      <td title={account.last_sign_in && formatDateTime(account.last_sign_in)}>
        {account.last_sign_in ? formatAgo(account.last_sign_in, now) : "Never"}
      </td>
      <td title={formatDateTime(account.created_at)}>
        {formatAgo(account.created_at, now)}
        {account.created_by && (
          <div className="small muted">by {account.created_by}</div>
        )}
      </td>
      <td>
        <div className="toolbar tight">
          <button
            type="button"
            className="button-quiet"
            disabled={busy}
            onClick={() =>
              run(() =>
                api.updateAccount(account.id, { disabled: !account.disabled }),
              )
            }
          >
            {account.disabled ? "Let them in" : "Suspend"}
          </button>
          <button
            type="button"
            className="button-quiet"
            disabled={busy}
            onClick={() => setChanging(!changing)}
            aria-expanded={changing}
          >
            New password
          </button>
          <button
            type="button"
            className="button-quiet"
            disabled={busy}
            onClick={() => run(() => api.deleteAccount(account.id))}
          >
            Remove
          </button>
        </div>
        {changing && (
          <form
            className="toolbar tight"
            onSubmit={(e: FormEvent) => {
              e.preventDefault();
              run(async () => {
                await api.setAccountPassword(account.id, password);
                setPassword("");
                setChanging(false);
              });
            }}
          >
            <label className="sr-only" htmlFor={`pw-${account.id}`}>
              New password for {account.username}
            </label>
            <input
              id={`pw-${account.id}`}
              type="password"
              autoComplete="new-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
            <button
              type="submit"
              className="button-primary"
              disabled={busy || password.length < 12}
            >
              Set it
            </button>
          </form>
        )}
      </td>
    </tr>
  );
}

function AddAccount({ onAdded }: { onAdded: () => void }) {
  const [open, setOpen] = useState(false);
  const [username, setUsername] = useState("");
  const [fullName, setFullName] = useState("");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState<Role>("viewer");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  async function add(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      await api.addAccount({
        username: username.trim(),
        password,
        full_name: fullName.trim() || undefined,
        role,
      });
      setUsername("");
      setFullName("");
      setPassword("");
      setRole("viewer");
      setOpen(false);
      onAdded();
    } catch (err) {
      setError(
        err instanceof ApiError ? err.message : "Adding the account failed.",
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="panel" aria-labelledby="add-account-heading">
      <div className="section-header">
        <h2 id="add-account-heading">Add an account</h2>
        <button
          type="button"
          className="button-quiet"
          aria-expanded={open}
          onClick={() => setOpen(!open)}
        >
          {open ? "Cancel" : "Add an account"}
        </button>
      </div>
      {!open ? (
        <p className="muted">
          For people who look after ForgeSync. A password of at least 12
          characters; the role decides what they may do here, and nothing else.
        </p>
      ) : (
        <form className="stack" onSubmit={add} noValidate>
          <div className="field">
            <label htmlFor="new-account-username">Username</label>
            <input
              id="new-account-username"
              value={username}
              autoComplete="off"
              onChange={(e) => setUsername(e.target.value)}
              required
            />
          </div>
          <div className="field">
            <label htmlFor="new-account-name">Full name (optional)</label>
            <input
              id="new-account-name"
              value={fullName}
              autoComplete="off"
              onChange={(e) => setFullName(e.target.value)}
            />
          </div>
          <div className="field">
            <label htmlFor="new-account-password">Password</label>
            <input
              id="new-account-password"
              type="password"
              autoComplete="new-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              required
            />
            <p className="small muted">
              At least 12 characters. It&rsquo;s stored as a hash and
              can&rsquo;t be read back, here or anywhere else.
            </p>
          </div>
          <div className="field">
            <label htmlFor="new-account-role">Role</label>
            <select
              id="new-account-role"
              value={role}
              onChange={(e) => setRole(e.target.value as Role)}
            >
              {ROLES.map((r) => (
                <option key={r} value={r}>
                  {r}
                </option>
              ))}
            </select>
          </div>
          {error && <ErrorNote message={error} />}
          <div className="toolbar tight">
            <button
              type="submit"
              className="button-primary"
              disabled={busy || username.trim() === "" || password.length < 12}
            >
              {busy ? "Adding…" : "Add the account"}
            </button>
          </div>
        </form>
      )}
    </section>
  );
}
