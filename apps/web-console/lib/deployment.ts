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

/** An unfamiliar word from a newer control plane is shown as itself rather than rounded off to a known one. */
export function mutabilityLabel(value: string): string {
  return MUTABILITY_LABEL[value] ?? value;
}
