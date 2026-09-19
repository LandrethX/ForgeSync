import { useEffect, useState, type FormEvent } from "react";
import { api, ApiError, type AuthConfig, type Session } from "../api";
import { formatDuration } from "../format";

/**
 * Signing in to ForgeSync is a ForgeSync account. SceneID says who may
 * use the Forgejo nodes; the people who look after the controllers are a
 * different set, and their accounts live in ForgeSync's own database,
 * which both controllers share. The admin token stays as the break-glass
 * way in, and is how the first account gets made.
 */
export function Login({ onSignedIn }: { onSignedIn: (s: Session) => void }) {
  const [config, setConfig] = useState<AuthConfig>();
  const [configError, setConfigError] = useState<string>();

  useEffect(() => {
    api
      .authConfig()
      .then(setConfig)
      .catch((e: unknown) =>
        setConfigError(e instanceof Error ? e.message : String(e)),
      );
  }, []);

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
        {configError && (
          <p className="error-note" role="alert">
            {configError}
          </p>
        )}
        <PasswordForm onSignedIn={onSignedIn} />
        {config?.token_sign_in && (
          <details className="break-glass">
            <summary>Use the admin token instead</summary>
            <p className="muted">
              For emergencies, and for making the first account. Every use is
              recorded in the audit log.
            </p>
            <TokenForm onSignedIn={onSignedIn} />
          </details>
        )}
      </div>
    </div>
  );
}

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
