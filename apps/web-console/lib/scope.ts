// THE CURRENT SCOPE — one key, two readers, and it exists so the shell's picker is not decorative.
//
// This console holds ONE server-side API key and every /v1 read is scoped by that key's tenant: there is no
// request parameter on /v1/agents, /v1/responses or /v1/usage that narrows a read to a project. So a project
// picker in the shell could easily have been a control that changes nothing — the exact defect shape this
// tree keeps finding in its own code, and the reason the picker renders only when there is more than one
// project to choose between.
//
// What it DOES change is the one screen that takes a project as a parameter: `/policy` writes a project's
// configuration document and mints keys against it, and it opens on whichever project was last chosen here.
// That is a small effect, and it is a real one — which is the difference between a control and a decoration.
//
// sessionStorage, not localStorage: a scope is a property of this sitting at the console, not a preference.
// A new tab starts on the deployment's first project, which is also what a fresh operator should get.
const KEY = "palai.console.project";

/** rememberedProject returns the project id chosen in this browser session, or "" when none was. */
export function rememberedProject(): string {
  // Guarded because this module is imported by components that render on the server during the build.
  if (typeof window === "undefined") return "";
  try {
    return window.sessionStorage.getItem(KEY) ?? "";
  } catch {
    // A browser with storage disabled is a browser that gets the default scope, not an error region.
    return "";
  }
}

/** rememberProject records the operator's choice for the rest of this browser session. */
export function rememberProject(id: string): void {
  if (typeof window === "undefined") return;
  try {
    window.sessionStorage.setItem(KEY, id);
  } catch {
    /* same rule: storage is a convenience here, never a correctness dependency */
  }
}
