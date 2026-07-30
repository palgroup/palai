import { test, expect } from "@playwright/test";

import { SecretField } from "../components/SecretField";
import { laneFor } from "../lib/timeline";

// The runnable check for the §47.2 lane table: if the mapping drifts (a tool event landing in the model
// lane, an approval folded into progress, a recovery transition mislabelled), the console's separation is
// broken and this fails. No browser needed — a pure-function assertion in the same `playwright test` run.
test("laneFor sorts canonical event types into the §47.2 lanes", () => {
  expect(laneFor("message.accepted.v1")).toBe("message");
  expect(laneFor("model_step.delta.v1")).toBe("model_step");
  expect(laneFor("tool_call.completed.v1")).toBe("tool");
  expect(laneFor("child.completed.v1")).toBe("tool");
  expect(laneFor("approval.requested.v1")).toBe("approval");
  expect(laneFor("approval.approved.v1")).toBe("approval");
  expect(laneFor("attempt.recovering.v1")).toBe("recovery");
  expect(laneFor("recovery.proof.v1")).toBe("recovery");
  expect(laneFor("workspace.restored.v1")).toBe("recovery");
  expect(laneFor("usage.updated.v1")).toBe("usage");
  expect(laneFor("response.in_progress.v1")).toBe("progress");
  expect(laneFor("run.completed.v1")).toBe("terminal");
  expect(laneFor("response.failed.v1")).toBe("terminal");
});

// PASTE IS NOT BLOCKED, AND THE CHECK READS THE COMPONENT'S PROPS (E25 T4, plan §3.5 N12 / WCAG 2.2 §3.3.8).
//
// It cannot be a DOM assertion: a paste handler leaves no attribute behind, so a rendered field that silently
// swallows Ctrl-V looks identical to one that does not. THE FIRST DRAFT OF THIS TEST SCANNED THE SOURCE TEXT
// FOR "onPaste" AND FAILED ON THE COMPONENT'S OWN COMMENT SAYING IT HAS NONE — the same defeat this tree has
// shipped four times with substring comparisons, arriving here as a false positive instead of a false green.
// So the element tree is walked and the input's ACTUAL props are read: a comment cannot be a prop.
//
// Blocking paste on a credential field means retyping forty random characters by eye into a value nobody can
// read back to verify — the cognitive test 3.3.8 exists to forbid, and a source of undetectable typos.
//
// It also pins the token, because `off` is the WRONG answer rather than a weaker one: browsers ignore it on
// password fields and MDN reserves it for CAPTCHA / one-time-token fields, while `new-password` is documented
// to prevent an EXISTING password being autofilled — here, the operator's own console password being dropped
// into a box whose contents an agent will then use as a credential.
test("SecretField does not block paste or copy, and asks for new-password rather than off", () => {
  const input = findNode(SecretField({ inputRef: { current: null }, label: "Value", testId: "value-secret-input" }), "input");
  // A shape change in the transform must FAIL rather than pass over nothing.
  expect(input, "no <input> was found in SecretField's element tree — this assertion would be vacuous").not.toBe(undefined);
  const props = input as Record<string, unknown>;

  expect(props.onPaste, "SecretField blocks paste — WCAG 2.2 §3.3.8 counts paste as a mechanism that SATISFIES it").toBe(undefined);
  expect(props.onCopy, "SecretField interferes with copy").toBe(undefined);
  expect(props.onCut, "SecretField interferes with cut").toBe(undefined);
  expect(props.onKeyDown, "SecretField intercepts keys — the only reason to would be to block one").toBe(undefined);
  expect(props.autoComplete).toBe("new-password");
  expect(props.type).toBe("password");
  // NO value and NO defaultValue: the secret is a DOM node, never React state. Either prop would put a
  // credential into the component tree and into every render's closure.
  expect(props.value, "SecretField is controlled — the secret would be React state").toBe(undefined);
  expect(props.defaultValue, "SecretField seeds the field with bytes").toBe(undefined);
});

/**
 * findNode walks a JSX element tree and returns the PROPS of the first node of `tag`.
 *
 * It reads `type`/`props`/`children` only, which both React elements and Playwright's JSX transform expose —
 * this file is a .ts, so the .tsx components it imports are compiled by whichever transform is active, and
 * the caller asserts that a node was actually found so a transform change cannot make this pass over nothing.
 */
function findNode(node: unknown, tag: string): Record<string, unknown> | undefined {
  if (Array.isArray(node)) {
    for (const child of node) {
      const hit = findNode(child, tag);
      if (hit !== undefined) return hit;
    }
    return undefined;
  }
  if (node === null || typeof node !== "object") return undefined;
  const el = node as { type?: unknown; props?: Record<string, unknown> };
  if (el.type === tag) return el.props ?? {};
  return el.props === undefined ? undefined : findNode(el.props.children, tag);
}
