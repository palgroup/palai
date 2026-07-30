"use client";

import { useState } from "react";

import { ResourceForm } from "@/components/ResourceForm";

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
//
// E25 T2 — THE MARKUP IS NOW components/ResourceForm.tsx AND THE FOUR RULES ABOVE LIVE THERE. This page was
// the console's only form and it hand-wrote all four; T2 establishes them once so T4/T5/T6's six forms inherit
// them. The refactor is verified by T1's OWN specs, unchanged: axe-clean under the widened tag set, the
// role="alert" refusal, current-password autofill, paste unblocked, keyboard-only submit. If ResourceForm had
// dropped any of them, those tests fail — which is why the first caller is a form that was already proven.
export default function LoginPage() {
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit() {
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
    <ResourceForm
      title="Sign in"
      testId="login"
      note={
        <span data-testid="login-note">
          This console holds a server-side API key with <strong>full</strong> control-plane authority, so it
          asks for the operator password before relaying anything. One operator, one password — there are no
          user accounts, no roles and no audit trail (see docs/operations/console.md).
        </span>
      }
      fields={[
        {
          name: "password",
          label: "Operator password",
          kind: "password",
          // current-password, not "off": the operator's password manager must be able to fill this (WCAG 2.2
          // §3.3.8), and browsers ignore "off" on a password field anyway.
          autoComplete: "current-password",
          required: true,
          value: password,
          onChange: setPassword,
          testId: "password-input",
        },
      ]}
      submitLabel="Sign in"
      submittingLabel="Signing in…"
      submitTestId="login-button"
      submitting={busy}
      error={error}
      onSubmit={submit}
    />
  );
}
