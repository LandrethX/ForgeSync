import { type FormEvent, useState } from "react";

import { ApiError, api, type NodeReport } from "../api";
import { StatusIcon } from "../components/StatusBadge";

/**
 * Adding a Forgejo node.
 *
 * ForgeSync does nothing to the node on your behalf. The parts that
 * matter most are `app.ini` keys, which no API can reach and which need
 * the node restarted, so this says what the node needs, you do it, and
 * then Check says what ForgeSync can actually see. That is the whole
 * shape: instructions, then verify.
 */
export function AddNode({ onAdded }: { onAdded: () => void }) {
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [url, setUrl] = useState("");
  const [site, setSite] = useState("");
  const [serviceUser, setServiceUser] = useState("forgesync");
  const [sourceId, setSourceId] = useState("");
  const [token, setToken] = useState("");
  const [report, setReport] = useState<NodeReport>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [added, setAdded] = useState<string>();

  function fields() {
    return {
      name: name.trim().toLowerCase(),
      url: url.trim().replace(/\/+$/, ""),
      site: site.trim() || undefined,
      service_user: serviceUser.trim() || undefined,
      sceneid_source_id: sourceId.trim() ? Number(sourceId.trim()) : undefined,
      token: token.trim(),
    };
  }

  async function run(what: "check" | "add", e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    setAdded(undefined);
    try {
      const rep =
        what === "check"
          ? await api.checkNode(fields())
          : await api.addNode(fields());
      setReport(rep);
      if (what === "add" && rep.ok) {
        setAdded(name.trim().toLowerCase());
        setToken("");
        onAdded();
      }
    } catch (err) {
      // A node that fails a blocking check comes back as 422 with the
      // whole report, which is what the person needs to see.
      if (err instanceof ApiError && isReport(err.body)) {
        setReport(err.body);
      } else {
        setError(
          err instanceof ApiError
            ? err.message
            : what === "check"
              ? "Checking the node failed."
              : "Adding the node failed.",
        );
      }
    } finally {
      setBusy(false);
    }
  }

  const host = url.trim() ? safeHost(url.trim()) : "the node";

  return (
    <section className="panel" aria-labelledby="add-node-heading">
      <div className="section-header">
        <h2 id="add-node-heading">Add a node</h2>
        <button
          type="button"
          className="button-quiet"
          aria-expanded={open}
          onClick={() => setOpen(!open)}
        >
          {open ? "Cancel" : "Add a node"}
        </button>
      </div>
      {!open ? (
        <p className="muted">
          A node is a Forgejo server ForgeSync keeps in step with the others. It
          needs a site-admin account there and a token for it; ForgeSync checks
          what it can reach before storing anything.
        </p>
      ) : (
        <>
          <Prepare host={host} />
          <form className="stack" onSubmit={(e) => run("add", e)} noValidate>
            <div className="field">
              <label htmlFor="node-name">Name</label>
              <input
                id="node-name"
                value={name}
                autoComplete="off"
                placeholder="se"
                onChange={(e) => setName(e.target.value)}
                required
              />
              <p className="muted small">
                Short and lowercase. It appears in the history, in conflicts and
                in URLs, and it cannot be changed afterwards.
              </p>
            </div>
            <div className="field">
              <label htmlFor="node-url">Address</label>
              <input
                id="node-url"
                value={url}
                autoComplete="off"
                placeholder="https://forgejo-se.example.org"
                onChange={(e) => setUrl(e.target.value)}
                required
              />
            </div>
            <div className="field">
              <label htmlFor="node-site">Site (optional)</label>
              <input
                id="node-site"
                value={site}
                autoComplete="off"
                placeholder="SE"
                onChange={(e) => setSite(e.target.value)}
              />
            </div>
            <div className="field">
              <label htmlFor="node-user">Service account</label>
              <input
                id="node-user"
                value={serviceUser}
                autoComplete="off"
                onChange={(e) => setServiceUser(e.target.value)}
              />
              <p className="muted small">
                The site admin on that node whose token this is. ForgeSync uses
                whoever the token turns out to belong to.
              </p>
            </div>
            <div className="field">
              <label htmlFor="node-source">SceneID login source id (optional)</label>
              <input
                id="node-source"
                value={sourceId}
                autoComplete="off"
                inputMode="numeric"
                placeholder="2"
                onChange={(e) => setSourceId(e.target.value)}
              />
              <p className="muted small">
                From <code>forgejo admin auth list</code> on the node. Without
                it ForgeSync cannot create SceneID accounts there, so a
                repository whose owner has never signed in on this node waits.
              </p>
            </div>
            <div className="field">
              <label htmlFor="node-token">API token</label>
              <input
                id="node-token"
                type="password"
                value={token}
                autoComplete="off"
                onChange={(e) => setToken(e.target.value)}
                required
              />
              <p className="muted small">
                Stored sealed with this installation's node key, never in the
                clear, and never shown again.
              </p>
            </div>
            <Actions
              busy={busy}
              onCheck={(e) => run("check", e)}
              canSubmit={Boolean(name.trim() && url.trim() && token.trim())}
            />
          </form>
        </>
      )}
      {error && <p className="error-note">{error}</p>}
      {added && (
        <p className="muted">
          <strong>{added}</strong> was added. The controllers pick it up within
          a few seconds, restarting to do so, which costs nothing: the work is
          idempotent and another controller keeps serving meanwhile.
        </p>
      )}
      {report && <Report report={report} />}
    </section>
  );
}

/**
 * Both buttons work on either controller. A node lives in the shared
 * database, like a ForgeSync account, so adding one is not the acting
 * controller's privilege and is not disabled on a standby.
 */
function Actions({
  busy,
  onCheck,
  canSubmit,
}: {
  busy: boolean;
  onCheck: (e: FormEvent) => void;
  canSubmit: boolean;
}) {
  return (
    <div className="row">
      <button type="button" onClick={onCheck} disabled={busy || !canSubmit}>
        {busy ? "Checking…" : "Check"}
      </button>
      <button
        type="submit"
        className="button-primary"
        disabled={busy || !canSubmit}
      >
        Add the node
      </button>
    </div>
  );
}

/** What the node itself needs, which ForgeSync cannot do for you. */
function Prepare({ host }: { host: string }) {
  return (
    <details className="stack">
      <summary>What {host} needs first</summary>
      <p className="muted small">
        ForgeSync never changes a node's own configuration: two of these live in{" "}
        <code>app.ini</code>, which no API can reach and which needs Forgejo
        restarted. Do these there, then press Check.
      </p>
      <ol className="muted small">
        <li>
          A site-admin account for ForgeSync, and an API token for it with all
          scopes:
          <pre>
            <code>
              forgejo admin user create --admin --username forgesync
              --email forgesync@example.org{"\n"}
              forgejo admin user generate-access-token --username forgesync \
              {"\n  "}--token-name forgesync --scopes all --raw
            </code>
          </pre>
        </li>
        <li>
          Let the node reach this controller, so it can report changes as they
          happen:
          <pre>
            <code>
              [webhook]{"\n"}ALLOWED_HOST_LIST = your-forgesync-host
            </code>
          </pre>
        </li>
        <li>
          Turn the LFS server on, or large files cannot be carried:
          <pre>
            <code>[server]{"\n"}LFS_START_SERVER = true</code>
          </pre>
        </li>
        <li>
          Restart Forgejo, and note the SceneID login source id from{" "}
          <code>forgejo admin auth list</code>.
        </li>
      </ol>
    </details>
  );
}

function Report({ report }: { report: NodeReport }) {
  return (
    <div className="stack" aria-live="polite">
      <h3>What ForgeSync found</h3>
      <ul className="findings">
        {report.findings.map((f) => (
          <li key={f.check}>
            <StatusIcon
              tone={f.ok ? "good" : f.blocking ? "serious" : "warning"}
            />{" "}
            <strong>{f.check}</strong>
            {f.detail && <span className="muted"> {f.detail}</span>}
          </li>
        ))}
      </ul>
      {!report.ok && (
        <p className="muted">
          Nothing was stored. Put the red ones right on the node and press Check
          again.
        </p>
      )}
    </div>
  );
}

function isReport(body: unknown): body is NodeReport {
  return (
    typeof body === "object" &&
    body !== null &&
    "findings" in body &&
    Array.isArray((body as { findings: unknown }).findings)
  );
}

function safeHost(raw: string): string {
  try {
    return new URL(raw).host || "the node";
  } catch {
    return "the node";
  }
}
