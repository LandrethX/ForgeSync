import { useEffect, useState } from "react";
import { ApiError, api, hasRole, type User } from "../api";
import { ErrorNote, PageHeader } from "../components/Layout";
import { formatDateTime } from "../format";
import { useDebounced, useLoad, useNodes, useSession } from "../hooks";

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
  const nodeNames = nodes?.map((n) => n.name) ?? [...new Set(list.data?.items.flatMap((u) => u.accounts.map((a) => a.node)))].sort();

  return (
    <>
      <PageHeader title="Users" />
      <p className="muted page-intro">
        SceneID users found on the nodes. Each has a primary site: by default where they registered, meaning the node
        their account was created on first. Their repositories take it as their primary. ForgeSync creates their account
        on another node when one of their repositories is copied there; they're linked to it on their first sign-in.
      </p>
      <div className="toolbar">
        <label htmlFor="user-search">Search</label>
        <input id="user-search" type="search" placeholder="login" value={query} onChange={(e) => setQuery(e.target.value)} />
      </div>
      {list.error && <ErrorNote message={list.error} />}
      {list.data && list.data.items.length === 0 && (
        <p className="muted">{q ? "No users match." : "No SceneID users found yet. The first scan may still be running."}</p>
      )}
      {list.data && list.data.items.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">User</th>
                <th scope="col">Primary site</th>
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
                    <HomeCell user={u} nodeNames={nodeNames} onSaved={list.reload} />
                  </td>
                  {nodeNames.map((n) => {
                    const a = u.accounts.find((x) => x.node === n && x.present);
                    return (
                      <td key={n} title={a?.forgejo_created_at ? `Created ${formatDateTime(a.forgejo_created_at)}` : undefined}>
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

function HomeCell({ user, nodeNames, onSaved }: { user: User; nodeNames: string[]; onSaved: () => void }) {
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
      <select id={id} value={choice} onChange={(e) => setChoice(e.target.value)}>
        {!user.home_node && <option value="">Not set yet</option>}
        {nodeNames.map((n) => (
          <option key={n} value={n}>
            {n}
          </option>
        ))}
      </select>
      {choice !== user.home_node && choice && (
        <button type="button" className="button-primary" onClick={save} disabled={busy}>
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
