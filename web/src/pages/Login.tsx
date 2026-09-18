import { useState, type FormEvent } from "react";
import { api, ApiError } from "../api";
import { formatDuration } from "../format";

export function Login({ onSignedIn }: { onSignedIn: () => void }) {
  const [token, setToken] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      await api.signIn(token.trim());
      setToken("");
      onSignedIn();
    } catch (err) {
      setError(messageFor(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="login">
      <form className="login-card" onSubmit={submit} noValidate>
        <div className="brand login-brand">
          <img src="/favicon.svg" alt="" width={28} height={28} />
          ForgeSync
        </div>
        <h1>Sign in</h1>
        <p className="muted">
          Use the controller's admin token (the file named by <code>http.admin_token_file</code>). SceneID sign-in
          will replace this.
        </p>
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
        <button type="submit" className="button-primary" disabled={busy || token.trim() === ""}>
          {busy ? "Signing in…" : "Sign in"}
        </button>
      </form>
    </div>
  );
}

function messageFor(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.status === 401) return "That token isn't valid.";
    if (err.status === 429) {
      const wait = err.retryAfterSeconds ? ` Try again in ${formatDuration(err.retryAfterSeconds * 1000)}.` : "";
      return `Too many failed attempts.${wait}`;
    }
    if (err.status === 503) return "The admin API is turned off on this controller (no admin token configured).";
    return err.message;
  }
  return "Sign-in failed.";
}
