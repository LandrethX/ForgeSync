import { useEffect, useState, type FormEvent } from "react";
import {
  api,
  ApiError,
  sceneIdLoginURL,
  type AuthConfig,
  type Session,
} from "../api";
import { formatDuration } from "../format";

// Explanations for /?signin_error=... set by the SceneID callback.
const SIGN_IN_ERRORS: Record<string, string> = {
  no_role:
    "Your SceneID account doesn't have a ForgeSync role. Ask an administrator to give you one in SceneID.",
  expired:
    "The sign-in took too long, or was started in another browser. Try again.",
  cancelled: "Sign-in was cancelled at SceneID.",
  unavailable: "Can't reach SceneID right now. Try again in a moment.",
  failed:
    "Sign-in failed. Try again, or ask an administrator to check the controller log.",
};

/** Reads and removes ?signin_error from the address bar, so a reload doesn't repeat it. */
function takeSignInError(): string | undefined {
  const params = new URLSearchParams(window.location.search);
  const code = params.get("signin_error");
  if (!code) return undefined;
  params.delete("signin_error");
  const rest = params.toString();
  window.history.replaceState(
    null,
    "",
    window.location.pathname + (rest ? `?${rest}` : ""),
  );
  return SIGN_IN_ERRORS[code] ?? SIGN_IN_ERRORS.failed;
}

export function Login({ onSignedIn }: { onSignedIn: (s: Session) => void }) {
  const [config, setConfig] = useState<AuthConfig>();
  const [configError, setConfigError] = useState<string>();
  const [sceneIdError] = useState(takeSignInError);

  useEffect(() => {
    api
      .authConfig()
      .then(setConfig)
      .catch((e: unknown) =>
        setConfigError(e instanceof Error ? e.message : String(e)),
      );
  }, []);

  const returnTo = window.location.pathname + window.location.search;

  return (
    <div className="login">
      <div className="login-mark">
        <img src="/logo.svg" alt="" width={132} height={132} />
        <p className="wordmark">
          Forge<span>Sync</span>
        </p>
      </div>
      <div className="login-card">
        <h1>Sign in</h1>
        {sceneIdError && (
          <p className="error-note" role="alert">
            {sceneIdError}
          </p>
        )}
        {configError && (
          <p className="error-note" role="alert">
            {configError}
          </p>
        )}
        <PasswordForm onSignedIn={onSignedIn} />
        {config?.sceneid && (
          <>
            <p className="muted or-line">
              Or sign in with your SceneID account, which is where your
              ForgeSync role comes from.
            </p>
            <a
              className="button-primary button-link"
              href={sceneIdLoginURL(returnTo)}
            >
              Sign in with SceneID
            </a>
          </>
        )}
        {config?.token_sign_in &&
          (config.sceneid ? (
            <details className="break-glass">
              <summary>Use the admin token instead</summary>
              <p className="muted">
                For emergencies when SceneID is unavailable. Every use is
                recorded in the audit log.
              </p>
              <TokenForm onSignedIn={onSignedIn} />
            </details>
          ) : (
            <>
              <p className="muted">
                Use the controller's admin token (the file named by{" "}
                <code>http.admin_token_file</code>).
              </p>
              <TokenForm onSignedIn={onSignedIn} />
            </>
          ))}
        {config && !config.sceneid && !config.token_sign_in && (
          <p className="muted">
            Web sign-in isn't configured on this controller.
          </p>
        )}
      </div>
    </div>
  );
}

/**
 * ForgeSync's own accounts. They're in the database both controllers
 * share, so this works on either one -- and when SceneID is the thing
 * that's unreachable, which is when someone most needs to get in.
 */
function PasswordForm({ onSignedIn }: { onSignedIn: (s: Session) => void }) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      onSignedIn(await api.signInWithPassword(username.trim(), password));
      setPassword("");
    } catch (err) {
      setError(
        messageFor(err, "That username and password don't match an account."),
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <form className="token-form" onSubmit={submit} noValidate>
      <label htmlFor="username">ForgeSync account</label>
      <input
        id="username"
        autoComplete="username"
        value={username}
        onChange={(e) => setUsername(e.target.value)}
        required
      />
      <label htmlFor="password">Password</label>
      <input
        id="password"
        type="password"
        autoComplete="current-password"
        value={password}
        onChange={(e) => setPassword(e.target.value)}
        aria-invalid={error ? true : undefined}
        aria-describedby={error ? "password-error" : undefined}
        required
      />
      {error && (
        <p id="password-error" className="error-note" role="alert">
          {error}
        </p>
      )}
      <button
        type="submit"
        className="button-primary"
        disabled={busy || username.trim() === "" || password === ""}
      >
        {busy ? "Signing in…" : "Sign in"}
      </button>
    </form>
  );
}

function TokenForm({ onSignedIn }: { onSignedIn: (s: Session) => void }) {
  const [token, setToken] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      const session = await api.signIn(token.trim());
      setToken("");
      onSignedIn(session);
    } catch (err) {
      setError(messageFor(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <form className="token-form" onSubmit={submit} noValidate>
      <label htmlFor="token">Admin token</label>
      <input
        id="token"
        type="password"
        autoComplete="current-password"
        value={token}
        onChange={(e) => setToken(e.target.value)}
        aria-invalid={error ? true : undefined}
        aria-describedby={error ? "login-error" : undefined}
        required
      />
      {error && (
        <p id="login-error" className="error-note" role="alert">
          {error}
        </p>
      )}
      <button
        type="submit"
        className="button-quiet"
        disabled={busy || token.trim() === ""}
      >
        {busy ? "Signing in…" : "Sign in with token"}
      </button>
    </form>
  );
}

/** `wrong` is what a 401 means for the form that asked. */
function messageFor(err: unknown, wrong = "That token isn't valid."): string {
  if (err instanceof ApiError) {
    if (err.status === 401) return wrong;
    if (err.status === 429) {
      const wait = err.retryAfterSeconds
        ? ` Try again in ${formatDuration(err.retryAfterSeconds * 1000)}.`
        : "";
      return `Too many failed attempts.${wait}`;
    }
    return err.message;
  }
  return "Sign-in failed.";
}
