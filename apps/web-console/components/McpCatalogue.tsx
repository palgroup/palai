"use client";

import { useState } from "react";

import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";
import {
  AUTH_LABEL,
  MCP_CATALOGUE,
  MCP_CATALOGUE_GROUPS,
  isConnectable,
  type CatalogueEntry,
} from "@/lib/mcpCatalogue";

// THE CATALOGUE PICKER — a directory of known MCP servers, in place of asking an operator to type a URL.
//
// IT SHOWS THE SERVERS IT CANNOT CONNECT, and that is the whole design rather than a concession. A directory
// that silently omitted them would answer "is Slack supported?" with absence, which an operator reads as
// "not listed yet" — the wrong conclusion, and one they would reach again in a month. A directory that
// offered them a Use button would answer it with a registration that succeeds and a discovery that 401s,
// which is worse: registering DIALS NOTHING (app/tools/page.tsx register()), so the failure surfaces one
// screen later, detached from the choice that caused it.
//
// So a refused entry is listed, greyed of its action, and carries the sentence that says what would have to
// change. lib/mcpCatalogue.ts holds the classification and every citation behind it.
//
// WHY A DIALOG AND NOT A PAGE: this is a picker for the form beneath it — choosing an entry fills the name
// and URL fields and returns the operator to the form with the credential question still in front of them.
// A route would have added a row to lib/routes.ts and to the E28 bundle's ledger for a control that exists
// to hand two strings to a form on THIS page.
//
// IT IS OPENED BY A PLAIN BUTTON, NEVER FROM A MENU. A dialog opened from a menu does not have focus for one
// frame — the menu restores focus to its trigger as it closes (measured on /policy: t0 outside, 16ms
// inside). Opening from an ordinary button sidesteps that entirely, and tests/mcp-catalogue.spec.ts asserts
// focus lands inside rather than assuming it.

const TONE = {
  none: "ok",
  bearer: "ok",
  basic: "ok",
  schemeAllowed: "ok",
  oauth: "warn",
  customScheme: "warn",
  customHeader: "warn",
} as const;

const GLYPH = {
  none: "●",
  bearer: "●",
  basic: "●",
  schemeAllowed: "●",
  oauth: "▲",
  customScheme: "▲",
  customHeader: "▲",
} as const;

function Entry({ entry, onUse }: { entry: CatalogueEntry; onUse: (e: CatalogueEntry) => void }) {
  const usable = isConnectable(entry);
  return (
    <li className="catalogue-entry" data-testid={`catalogue-entry-${entry.id}`}>
      <div className="catalogue-entry-head">
        <div>
          <h4 data-testid={`catalogue-name-${entry.id}`}>{entry.name}</h4>
          <p className="muted">{entry.blurb}</p>
        </div>
        <Badge
          tone={TONE[entry.auth]}
          glyph={GLYPH[entry.auth]}
          title={entry.auth}
          testId={`catalogue-auth-${entry.id}`}
        >
          {AUTH_LABEL[entry.auth]}
        </Badge>
      </div>

      <p className="catalogue-url">
        <code>{entry.url}</code>
      </p>

      {/* The credential question, answered BEFORE the operator commits — registering dials nothing, so this
          is the last moment at which the answer is cheap. */}
      {usable && entry.credential !== undefined ? (
        <p className="muted" data-testid={`catalogue-credential-${entry.id}`}>
          <strong>Credential:</strong> {entry.credential}
        </p>
      ) : null}

      {!usable && entry.refusal !== undefined ? (
        <p className="catalogue-refusal" data-testid={`catalogue-refusal-${entry.id}`}>
          {entry.refusal}
        </p>
      ) : null}

      {entry.caveat !== undefined ? (
        <p className="muted" data-testid={`catalogue-caveat-${entry.id}`}>
          {entry.caveat}
        </p>
      ) : null}

      <p className="muted catalogue-evidence">
        Classified from{" "}
        <a href={entry.evidence} rel="noreferrer noopener" target="_blank">
          the vendor&apos;s own documentation
        </a>
        , which says: &ldquo;{entry.quote}&rdquo;
      </p>

      {usable ? (
        <Button variant="secondary" testId={`catalogue-use-${entry.id}`} onClick={() => onUse(entry)}>
          Use {entry.name}
        </Button>
      ) : null}
    </li>
  );
}

export function McpCatalogue({ onPick }: { onPick: (entry: CatalogueEntry) => void }) {
  const [open, setOpen] = useState(false);
  const ready = MCP_CATALOGUE.filter(isConnectable).length;

  return (
    <>
      <Button variant="secondary" testId="catalogue-open" onClick={() => setOpen(true)}>
        Browse known MCP servers
      </Button>

      <Dialog
        open={open}
        onOpenChange={setOpen}
        testId="catalogue-dialog"
        title="Known MCP servers"
        description={
          <>
            {ready} of {MCP_CATALOGUE.length} can be connected from this deployment. The rest are listed with
            the reason they cannot — this deployment sends a credential only as{" "}
            <code>Authorization: Bearer</code> or <code>Authorization: Basic</code>, and runs no OAuth flow,
            so a server needing an interactive sign-in has no way in. Choosing an entry fills the form
            behind this dialog; it registers nothing.
          </>
        }
        actions={
          <Button variant="secondary" testId="catalogue-close" onClick={() => setOpen(false)}>
            Close
          </Button>
        }
      >
        <div className="catalogue-body">
          {MCP_CATALOGUE_GROUPS.map((group) => {
            const entries = MCP_CATALOGUE.filter(group.match);
            return (
              <section key={group.id} data-testid={`catalogue-group-${group.id}`}>
                <h3>
                  {group.heading} ({entries.length})
                </h3>
                <p className="muted">{group.blurb}</p>
                <ul className="catalogue-list">
                  {entries.map((entry) => (
                    <Entry
                      key={entry.id}
                      entry={entry}
                      onUse={(e) => {
                        onPick(e);
                        setOpen(false);
                      }}
                    />
                  ))}
                </ul>
              </section>
            );
          })}
        </div>
      </Dialog>
    </>
  );
}
