"use client";

import { useCallback, useEffect, useState } from "react";

// SCREEN STATE THAT LIVES IN THE ADDRESS BAR, for the pages that are not a dynamic route.
//
// app/sessions/[id]/page.tsx put the first two parameters in this console into the URL (`?event=`,
// `?segment=`) and named the three things that follow: the back button undoes a selection, a screen can be
// sent to somebody as a link, and a reload lands where the reader was. Every other screen here still kept its
// selection in React state, so `/policy` always opened on whichever project the shell last remembered and
// `/fleet` always opened on the first pool — neither could be linked to, and the overview's project rows had
// nowhere to point.
//
// IT IS NOT next/navigation's useSearchParams, AND THAT IS A BUILD CONSTRAINT RATHER THAN A PREFERENCE.
// `/policy` and `/fleet` are STATICALLY PRERENDERED (`next build` marks both `○ (Static)`), and a client
// component calling useSearchParams inside a prerendered route has to sit under a Suspense boundary — whose
// FALLBACK is what lands in the served HTML. tests/auth.spec.ts fetches every declared route and asserts
// `method="post"` on every form tag in the bytes the server sent; a form that only exists after hydration is
// a form that sweep can no longer see, on the two pages that carry three of the console's ten. So the
// parameter is read from `window.location` in an effect, which leaves the server-rendered markup exactly as
// it was.
//
// popstate IS LISTENED FOR, because that is the half that makes the back button real. `history.replaceState`
// and `pushState` do not fire it themselves — only a user-driven history move does, which is precisely the
// event this has to answer.

/**
 * useQueryParam reads one query parameter and writes it back, keeping the two in step.
 *
 * The returned value is "" before the first effect runs (server render and first paint), so a caller must
 * treat "" as "not chosen yet" rather than as a choice — which is what both callers already do: each picks a
 * default once its collection has loaded, and an id the deployment does not have is ignored.
 *
 * `history` chooses which kind of history entry a write makes, and the two callers want different ones:
 *   "replace" — the write also happens on ARRIVAL, when the screen picks a default for a URL that named
 *               none. A push there would put a redirect nobody asked for into the browser's history, and
 *               Back would appear to do nothing.
 *   "push"    — the write only ever happens because somebody chose something, so Back should undo it.
 */
export function useQueryParam(name: string, history: "push" | "replace" = "replace"): [string, (next: string) => void] {
  const [value, setValue] = useState("");

  useEffect(() => {
    const read = () => setValue(new URLSearchParams(window.location.search).get(name) ?? "");
    read();
    window.addEventListener("popstate", read);
    return () => window.removeEventListener("popstate", read);
  }, [name]);

  const write = useCallback(
    (next: string) => {
      const params = new URLSearchParams(window.location.search);
      // DELETED RATHER THAN EMPTIED. `?project=` with nothing after it is a URL that says a project is
      // selected and does not say which — app/sessions/[id]/page.tsx's rule, and the same reasoning.
      if (next === "") params.delete(name);
      else params.set(name, next);
      const query = params.toString();
      const url = query === "" ? window.location.pathname : `${window.location.pathname}?${query}`;
      if (history === "push") window.history.pushState(null, "", url);
      else window.history.replaceState(null, "", url);
      setValue(next);
    },
    [name, history],
  );

  return [value, write];
}
