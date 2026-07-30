"use client";

import { useState } from "react";

// The console's sign-in form. A Client Component with a plain fetch, NOT a Server Action — deliberately, and
// at a cost that is written down rather than absorbed: a Server Action would get CSRF protection from the
// framework for free (§3.5 N8), but it would also be a second write path that the public-API-only network
// proof cannot see. So the protection is written by hand instead (SameSite=Strict plus an Origin comparison
// in lib/session.ts), and every write the browser can make still goes through a route the intercept sees.
//
// ACCESSIBILITY, and why each part is here rather than nice to have:
//   - a programmatic label (htmlFor/id), so the field is announced, not inferred from placement;
//   - autocomplete="current-password", which is what WCAG 2.2's 3.3.8 Accessible Authentication asks for:
//     the operator's password manager must be able to fill this, because a memory test is the failure this
//     criterion names. Paste is NOT blocked, for the same reason;
//   - a role="alert" region for the refusal, so a screen reader hears it without moving focus — which is
//     also why this is a fetch rather than a form POST + redirect: a full page load has nothing to announce;
//   - the refusal is TEXT, never colour alone.
export default function LoginPage() {
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      const res = await fetch("/api/console/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ password }),
      });
      if (res.status === 204) {
        // A full navigation, not a client transition: the session cookie has just changed and every panel
        // needs to be fetched again under it.
        window.location.assign("/");
        return;
      }
      const body = (await res.json().catch(() => ({}))) as { detail?: string };
      // Whatever the server said, it says the same thing for every refusal — this console has ONE
      // credential, so there is no half to be wrong about.
      setError(body.detail ?? "the password was not accepted");
    } catch {
      setError("the console could not be reached");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="panel" style={{ maxWidth: "26rem" }}>
      <h2>Sign in</h2>
      <p className="muted" data-testid="login-note">
        This console holds a server-side API key with <strong>full</strong> control-plane authority, so it
        asks for the operator password before relaying anything. One operator, one password — there are no
        user accounts, no roles and no audit trail (see docs/operations/console.md).
      </p>
      <form onSubmit={submit} data-testid="login-form">
        <label htmlFor="console-password">Operator password</label>
        <input
          id="console-password"
          name="password"
          type="password"
          autoComplete="current-password"
          required
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          data-testid="password-input"
        />
        {error === "" ? null : (
          <p role="alert" className="form-error" data-testid="login-error">
            <span className="glyph" aria-hidden="true">
              ✖
            </span>{" "}
            {error}
          </p>
        )}
        <p>
          <button type="submit" disabled={busy} data-testid="login-button">
            {busy ? "Signing in…" : "Sign in"}
          </button>
        </p>
      </form>
    </div>
  );
}
