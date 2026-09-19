import { useEffect, useState, type FormEvent } from "react";
import { ApiError, api, hasRole, type User, type UserWrite } from "../api";
import { ErrorNote, PageHeader } from "../components/Layout";
import { formatDateTime } from "../format";
import {
  useDebounced,
  useLeadership,
  useLoad,
  useNodes,
  useSession,
} from "../hooks";

const SOURCE_TEXT: Record<User["home_source"], string> = {
  "": "Not set yet",
  registration: "Where they registered",
  manual: "Chosen by an administrator",
};

export function Users() {
  const { nodes } = useNodes();
  const [query, setQuery] = useState("");
  const q = useDebounced(query.trim(), 250);
  const list = useLoad(() => api.users(q || undefined), [q]);
  const nodeNames =
    nodes?.map((n) => n.name) ??
    [
      ...new Set(
        list.data?.items.flatMap((u) => u.accounts.map((a) => a.node)),
      ),
    ].sort();

  return (
    <>
      <PageHeader title="Users" />
      <p className="muted page-intro">
        SceneID users found on the nodes. Each has a primary site: by default
        where they registered, meaning the node their account was created on
        first. Their repositories take it as their primary. ForgeSync creates
        their account on another node when one of their repositories is copied
        there; they're linked to it on their first sign-in. An administrator can
        add someone before any of that, so they can be given access to a
        repository before their first visit.
      </p>
      <AddUser nodeNames={nodeNames} onAdded={list.reload} />
      <div className="toolbar">
        <label htmlFor="user-search">Search</label>
        <input
          id="user-search"
          type="search"
          placeholder="login"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
        />
      </div>
      {list.error && <ErrorNote message={list.error} />}
      {list.data && list.data.items.length === 0 && (
        <p className="muted">
          {q
            ? "No users match."
            : "No SceneID users found yet. The first scan may still be running."}
        </p>
      )}
      {list.data && list.data.items.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">User</th>
                <th scope="col">Primary site</th>
                <th scope="col">On every node</th>
                {nodeNames.map((n) => (
                  <th scope="col" key={n}>
                    {n}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {list.data.items.map((u) => (
                <tr key={u.id}>
                  <th scope="row">{u.login}</th>
                  <td>
                    <HomeCell
                      user={u}
                      nodeNames={nodeNames}
                      onSaved={list.reload}
                    />
                  </td>
                  <td>
                    <MissingCell
                      user={u}
                      nodeNames={nodeNames}
                      onDone={list.reload}
                    />
                  </td>
                  {nodeNames.map((n) => {
                    const a = u.accounts.find((x) => x.node === n && x.present);
                    return (
                      <td
                        key={n}
                        title={
                          a?.forgejo_created_at
                            ? `Created ${formatDateTime(a.forgejo_created_at)}`
                            : undefined
                        }
                      >
                        {!a ? (
                          <span className="muted">No account</span>
                        ) : a.created_by_forgesync ? (
                          "Created by ForgeSync"
                        ) : (
                          "Registered"
                        )}
                      </td>
                    );
                  })}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {list.data && <p className="muted pager">{list.data.total} users</p>}
    </>
  );
}

/**
 * Adding a user, for Administrators. People come from SceneID, so this
 * doesn't make a local account: it creates the Forgejo account they
 * would get at their first sign-in, on every node at once, linked by
 * their SceneID subject. It's for someone who hasn't signed in yet, and
 * for getting them onto nodes before anything of theirs is copied there.
 */
function AddUser({
  nodeNames,
  onAdded,
}: {
  nodeNames: string[];
  onAdded: () => void;
}) {
  const session = useSession();
  const { readOnly, leaderName } = useLeadership();
  const [open, setOpen] = useState(false);
  const [login, setLogin] = useState("");
  const [subject, setSubject] = useState("");
  const [fullName, setFullName] = useState("");
  const [email, setEmail] = useState("");
  const [home, setHome] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [result, setResult] = useState<UserWrite>();

  if (!hasRole(session, "administrator")) return null;
  const homeNode = home || nodeNames[0] || "";

  async function add(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    setResult(undefined);
    try {
      const done = await api.addUser({
        login: login.trim(),
        subject: subject.trim(),
        full_name: fullName.trim() || undefined,
        email: email.trim() || undefined,
        home: homeNode,
      });
      setResult(done);
      setLogin("");
      setSubject("");
      setFullName("");
      setEmail("");
      onAdded();
    } catch (err) {
      setError(
        err instanceof ApiError ? err.message : "Adding the user failed.",
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="panel" aria-labelledby="add-user-heading">
      <div className="section-header">
        <h2 id="add-user-heading">Add a user</h2>
        <button
          type="button"
          className="button-quiet"
          aria-expanded={open}
          onClick={() => setOpen(!open)}
        >
          {open ? "Cancel" : "Add a user"}
        </button>
      </div>
      {!open ? (
        <p className="muted">
          People sign in with SceneID, so ForgeSync doesn&rsquo;t hold
          passwords. Adding someone here creates the account they would get at
          their first sign-in &mdash; on every node at once, linked by their
          SceneID subject &mdash; so they can be given access before they ever
          visit.
        </p>
      ) : (
        <form className="stack" onSubmit={add} noValidate>
          <p className="muted">
            The subject is the SceneID <code>sub</code> for that person. It is
            what links the account to them when they sign in; a wrong one leaves
            an account nobody can use. No password is set, here or on the nodes.
          </p>
          <p className="muted">
            This adds someone who uses the Forgejo nodes. ForgeSync&rsquo;s own
            administrators are administrators of the controllers &mdash; their
            role comes from SceneID and stays here &mdash; so they get no
            account on a node, and the local accounts a node already has (its
            site admin, ForgeSync&rsquo;s service account) are never
            ForgeSync&rsquo;s to create.
          </p>
          <div className="field">
            <label htmlFor="new-user-login">Username</label>
            <input
              id="new-user-login"
              value={login}
              onChange={(e) => setLogin(e.target.value)}
              autoComplete="off"
              required
            />
          </div>
          <div className="field">
            <label htmlFor="new-user-subject">SceneID subject</label>
            <input
              id="new-user-subject"
              value={subject}
              onChange={(e) => setSubject(e.target.value)}
              autoComplete="off"
              required
            />
          </div>
          <div className="field">
            <label htmlFor="new-user-name">Full name (optional)</label>
            <input
              id="new-user-name"
              value={fullName}
              onChange={(e) => setFullName(e.target.value)}
              autoComplete="off"
            />
          </div>
          <div className="field">
            <label htmlFor="new-user-email">E-mail (optional)</label>
            <input
              id="new-user-email"
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              autoComplete="off"
            />
          </div>
          <div className="field">
            <label htmlFor="new-user-home">Primary site</label>
            <select
              id="new-user-home"
              value={homeNode}
              onChange={(e) => setHome(e.target.value)}
            >
              {nodeNames.map((n) => (
                <option key={n} value={n}>
                  {n}
                </option>
              ))}
            </select>
            <p className="small muted">
              Their repositories take this node as their primary. The account
              there counts as where they registered; the others are
              ForgeSync&rsquo;s copies of it.
            </p>
          </div>
          {error && <ErrorNote message={error} />}
          <div className="toolbar tight">
            <button
              type="submit"
              className="button-primary"
              disabled={busy || readOnly || !login.trim() || !subject.trim()}
              title={
                readOnly
                  ? `Only ${leaderName || "the controller in charge"} can create accounts`
                  : undefined
              }
            >
              {busy ? "Adding…" : "Add the user"}
            </button>
          </div>
        </form>
      )}
      {result && <UserWriteNote result={result} />}
    </section>
  );
}

/**
 * Whether the person has an account everywhere, and for Administrators a
 * way to put that right. Without an account on a node they can't be made
 * a collaborator there, and nothing of theirs can be created, so a user
 * missing on a node is worth seeing and fixing here.
 */
function MissingCell({
  user,
  nodeNames,
  onDone,
}: {
  user: User;
  nodeNames: string[];
  onDone: () => void;
}) {
  const session = useSession();
  const { readOnly, leaderName } = useLeadership();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [result, setResult] = useState<UserWrite>();

  const missing = nodeNames.filter(
    (n) => !user.accounts.some((a) => a.node === n && a.present),
  );
  if (missing.length === 0) return <span className="muted">Yes</span>;

  async function copy() {
    setBusy(true);
    setError(undefined);
    try {
      setResult(await api.provisionUser(user.id));
      onDone();
    } catch (e) {
      setError(
        e instanceof ApiError ? e.message : "Copying the account failed.",
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <span>Missing on {missing.join(", ")}</span>
      {hasRole(session, "administrator") && (
        <div className="toolbar tight">
          <button
            type="button"
            className="button-quiet"
            onClick={copy}
            disabled={busy || readOnly}
            title={
              readOnly
                ? `Only ${leaderName || "the controller in charge"} can create accounts`
                : `Create ${user.login}'s account on ${missing.join(", ")}`
            }
          >
            {busy ? "Creating…" : "Create there"}
          </button>
        </div>
      )}
      {error && <p className="error-inline small">{error}</p>}
      {result && <UserWriteNote result={result} />}
    </>
  );
}

/** What happened on each node, after adding or copying an account. */
function UserWriteNote({ result }: { result: UserWrite }) {
  const refused = Object.entries(result.refused ?? {});
  return (
    <div role="status">
      <p className="muted">
        {result.created.length > 0
          ? `${result.login} was created on ${result.created.join(", ")}.`
          : `${result.login} was already on every node that would take them.`}
      </p>
      {refused.length > 0 && (
        <ul className="small">
          {refused.map(([node, why]) => (
            <li key={node}>
              <strong>{node}:</strong> {why}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function HomeCell({
  user,
  nodeNames,
  onSaved,
}: {
  user: User;
  nodeNames: string[];
  onSaved: () => void;
}) {
  const session = useSession();
  const [choice, setChoice] = useState(user.home_node);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  useEffect(() => setChoice(user.home_node), [user.home_node]);

  if (!hasRole(session, "administrator")) {
    return (
      <>
        {user.home_node || <span className="muted">Not set yet</span>}
        <span className="muted"> · {SOURCE_TEXT[user.home_source]}</span>
      </>
    );
  }
  const { readOnly, leaderName } = useLeadership();

  async function save() {
    setBusy(true);
    setError(undefined);
    try {
      await api.setUserHome(user.id, choice);
      onSaved();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Saving failed.");
    } finally {
      setBusy(false);
    }
  }
  const id = `home-${user.id}`;
  return (
    <div className="toolbar tight">
      <label htmlFor={id} className="sr-only">
        Primary site for {user.login}
      </label>
      <select
        id={id}
        value={choice}
        onChange={(e) => setChoice(e.target.value)}
      >
        {!user.home_node && <option value="">Not set yet</option>}
        {nodeNames.map((n) => (
          <option key={n} value={n}>
            {n}
          </option>
        ))}
      </select>
      {choice !== user.home_node && choice && (
        <button
          type="button"
          className="button-primary"
          onClick={save}
          disabled={busy || readOnly}
          title={
            readOnly
              ? `Only ${leaderName || "the controller in charge"} can change this`
              : undefined
          }
        >
          {busy ? "Saving…" : "Save"}
        </button>
      )}
      <span className="muted">{SOURCE_TEXT[user.home_source]}</span>
      {error && (
        <span role="alert" className="error-inline">
          {error}
        </span>
      )}
    </div>
  );
}
