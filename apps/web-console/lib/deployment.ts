// THE MACHINE'S CONFIGURATION, AS THE CONSOLE READS IT.
//
// GET /v1/deployment (apps/control-plane/api/deployment.go) reports the effective configuration of the
// control-plane PROCESS: what each setting is, what the process uses when it is unset, whether a new value
// needs the process replaced, and what changes it if not.
//
// WHY THE CONSOLE OWNS THE SCREEN MAP AND THE API DOES NOT. The API raises a WARNING when a configured
// value changes what the product does — but it names no console path, because which screen would lie is a
// question only the console can answer: it is the only thing that knows what its screens claim. A control
// plane carrying a list of "/runs" and "/history" would be a server with an opinion about a client it never
// serves. The `code` is the join, and the map below is this console's half of it.

/** One row of the effective configuration, exactly as api/deployment.go's deploymentSetting serialises. */
export interface DeploymentSetting extends Record<string, unknown> {
  name: string;
  group: string;
  value: string;
  set: boolean;
  /** What the process uses when the variable is unset, in the words of the code that applies it. */
  default: string;
  /** "value" or "path" — a *_FILE variable's value is a filesystem handle, and a handle is not a secret. */
  kind: string;
  effect: string;
  /** "bring_up" or "bring_up_default_only". See MUTABILITY_LABEL. */
  mutability: string;
  change_with: string;
  reader_file: string;
  reader_func: string;
  /**
   * DERIVED IN THE BROWSER, never sent by the API: true when this row's `change_with` is the sentence MOST
   * rows carry, so /deployment can print that one above the table instead of twenty-nine times inside it.
   * Set by markSharedRemedy in app/deployment/page.tsx, which is the only reader.
   */
  change_with_is_shared?: boolean;
  /**
   * Whether the panel may write a DESIRED value for this setting. SERVED, never derived here: which of the
   * thirty-five are writable is a decision the control plane's own catalogue makes (an allow-list that
   * drops every filesystem path structurally), and a console with its own copy of that list would be a
   * second opinion about a security boundary. A deployment older than this console sends nothing, which
   * reads as `false` and renders no control — the safe direction.
   */
  writable?: boolean;
  /** The catalogue's sentence for why this setting is NOT writable. Twenty-four of thirty-five carry one. */
  not_writable_because?: string;
  /** The shape a written value must have — "integer" | "rate" | "duration" | "token". See DESIRED_HELP. */
  value_grammar?: string;
  /** What the desired document asks for. `desired_set` distinguishes "decided" from "no opinion". */
  desired?: string;
  desired_set?: boolean;
  /** desired_set && desired !== value: this process is not running what was saved, so a bring-up is pending. */
  drift?: boolean;
}

/**
 * The desired document's own metadata. NULL — never an empty object — when no operator has ever written
 * one, and the screen says so in words: a machine nobody has configured from the panel is running on its
 * compose file's defaults, and an empty object here would let this console imply it is in control when it
 * is not.
 */
export interface DeploymentDesired {
  /**
   * WHICH PROCESS this document configures. `control_plane` is the singleton every project and every
   * machine on the deployment shares; `runner_pool` scopes a pool's machines and this deployment cannot
   * apply one yet (nothing hands cmd/runner a document — it reads its environment at exec).
   *
   * It is SERVED rather than assumed, and that is the whole reason the field exists: a screen that could
   * not tell the scope would render process-wide settings under a heading a reader takes as "this
   * machine", which is wrong about which machines a change reaches and about how many there are.
   */
  plane?: string;
  scope_id?: string;
  revision: number;
  written_at: string;
  written_by: string;
  pending: boolean;
  drifted: string[] | null;
}

export interface DeploymentWarning {
  code: string;
  severity: string;
  headline: string;
  detail: string;
  remedy: string;
  settings: string[];
}

export interface DeploymentBody {
  object?: string;
  settings?: DeploymentSetting[];
  warnings?: DeploymentWarning[];
  desired?: DeploymentDesired | null;
}

/**
 * WHICH SCREENS EACH WARNING BELONGS ON, and it is a short list on purpose.
 *
 * A banner repeated on every screen is chrome — a reader stops seeing it, which is the failure mode of the
 * thing it is trying to prevent. A warning goes where the console would otherwise make a claim the
 * configuration contradicts:
 *
 *   /runs      offers "Start a run and watch it happen". With no dispatcher nothing happens, and with the
 *              fake adapter what happens is fabricated. This is the screen that cost the evening.
 *   /deployment is where every warning appears, because that screen's whole subject is the configuration.
 *
 * /history is deliberately NOT here. A run with no dispatcher is shown there as `queued`, which is the
 * truth — the screen reports a state rather than promising a result.
 *
 * The paths are asserted against CONSOLE_ROUTES by tests/unit.spec.ts, so a renamed route unhooks the
 * banner visibly rather than silently.
 */
export const WARNING_SCREENS: Readonly<Record<string, readonly string[]>> = {
  dispatch_workers_zero: ["/deployment", "/runs"],
  model_provider_fake: ["/deployment", "/runs"],
  /**
   * A saved configuration this machine is not running. ONLY /deployment, and the narrowness is the point:
   * unlike the two above it does not make any other screen's claim untrue — /runs still starts runs and
   * /history still reports what happened. What is pending is a change an operator asked for, and the screen
   * whose subject is the configuration is where that belongs. Putting it everywhere would make it chrome,
   * which is the failure mode of the thing it is trying to prevent.
   */
  desired_config_pending: ["/deployment"],
};

/** warningsFor selects the warnings a given console path is responsible for showing. */
export function warningsFor(path: string, warnings: readonly DeploymentWarning[]): DeploymentWarning[] {
  return warnings.filter((w) => (WARNING_SCREENS[w.code] ?? []).includes(path));
}

/**
 * MUTABILITY_LABEL turns the API's vocabulary into the sentence an operator needs, and there are only two
 * entries because the code offers only two answers.
 *
 * There is no "editable" word and no edit control anywhere on this screen. Every setting the API reports is
 * read from the control-plane PROCESS's environment, which is fixed at exec — so a field that looked
 * editable and needed a restart to take effect would be exactly the "declared, and nothing happens" defect
 * this tree keeps finding, shipped into the one screen built to expose it.
 */
export const MUTABILITY_LABEL: Readonly<Record<string, string>> = {
  bring_up: "Needs a bring-up",
  bring_up_default_only: "Default only — overridable live",
};

/**
 * DESIRED_HELP is what each writable setting's field says under its label, and it is the console's ONE
 * piece of copy about a value grammar. It is deliberately not a validation rule: the control plane refuses
 * a value its own reader would coerce (`10min` is not a Go duration and would silently become the default),
 * and this console does not re-implement that check — it says what shape to type and shows the server's
 * refusal verbatim when it gets one. A second validator here would be a second opinion that drifts.
 */
export const DESIRED_HELP: Readonly<Record<string, string>> = {
  integer: "a whole number, 0 or more",
  rate: "a number, 0 or more (0 means unbounded)",
  duration: "a Go duration — 30s, 15m, 1h30m, 720h. `10min` is not one",
  token: "letters, digits and . _ : / - only",
};

/** An unfamiliar word from a newer control plane is shown as itself rather than rounded off to a known one. */
export function mutabilityLabel(value: string): string {
  return MUTABILITY_LABEL[value] ?? value;
}
