"use client";

import { useEffect, useState } from "react";

import { apiGet } from "@/lib/api";
import { warningsFor, type DeploymentBody, type DeploymentWarning } from "@/lib/deployment";

// THE BANNER ON THE SCREEN THAT WOULD OTHERWISE LIE.
//
// Measured, 2026-08-01: a stack brought up with `make local-up` takes PALAI_DISPATCH_WORKERS=0 from
// deploy/compose/compose.yaml:82. The console accepted five runs through this page and every one sat at
// run.queued.v1 forever, because startDispatch had returned before building a worker. NOTHING ON ANY SCREEN
// SAID SO — the value was eventually found with `docker inspect`. A console that accepts work its stack
// cannot perform, and says nothing, is worse than one that refuses.
//
// IT FAILS SILENT, AND THAT IS THE ONE JUDGEMENT WORTH DEFENDING. If GET /v1/deployment errors — an older
// control plane with no such route, a key without the `provision` capability — this renders nothing. The
// alternative is an error strip about a diagnostic on every page that carries one, which trains a reader to
// ignore the region where the real warning will appear. The screen is no worse off than before this
// component existed; a broken diagnostic that shouts is worse than one that is quiet.
//
// reloadKey (E29) is how a screen that CHANGES the deployment tells this to look again. /deployment's
// desired-configuration panel writes a document, which can raise or clear the `desired_config_pending`
// warning — and a banner that only reflects the state at page load would report a pending bring-up after
// the operator cleared it, or stay silent after they created one. Every other caller passes nothing and
// keeps the mount-once behaviour it had.
export function DeploymentNotice({ path, reloadKey = 0 }: { path: string; reloadKey?: number }) {
  const [warnings, setWarnings] = useState<DeploymentWarning[]>([]);

  useEffect(() => {
    let live = true;
    apiGet<DeploymentBody>("/deployment")
      .then((body) => {
        if (live) setWarnings(warningsFor(path, body.warnings ?? []));
      })
      .catch(() => {
        // See the header: a diagnostic that cannot read its own source says nothing.
      });
    return () => {
      live = false;
    };
  }, [path, reloadKey]);

  if (warnings.length === 0) return null;
  return (
    <div data-testid="deployment-notice">
      {warnings.map((w) => (
        <DeploymentWarningBlock key={w.code} warning={w} />
      ))}
    </div>
  );
}

// One warning, rendered in the shape this console already uses for a multi-sentence outcome: `.form-error`
// for the danger band, `.form-status[data-glyph="warn"]` for amber. Both are existing rules and neither is
// touched here — a screen added mid-redesign should not also be adding stylesheet.
//
// THE GLYPH AND THE WORD BOTH CARRY THE MEANING, never colour alone (UI-001, §47.5): the severity is written
// out in the heading text, so a colourblind reader and a screen reader get the same distinction a sighted
// one does from the band.
export function DeploymentWarningBlock({ warning }: { warning: DeploymentWarning }) {
  const blocking = warning.severity === "blocking";
  return (
    <div
      // role="status" rather than "alert": this is a standing property of the deployment, not an event that
      // just happened, and an assertive live region would interrupt a screen-reader user mid-sentence on
      // every page load.
      role="status"
      className={blocking ? "form-error" : "form-status"}
      data-glyph={blocking ? undefined : "warn"}
      data-testid={`deployment-warning-${warning.code}`}
      data-severity={warning.severity}
    >
      <span className="glyph" aria-hidden="true">
        {blocking ? "✖︎" : "⊘"}
      </span>
      <span>
        <strong>{blocking ? "Blocking: " : "Advisory: "}</strong>
        {warning.headline} {warning.detail}{" "}
        <strong>What changes it:</strong> {warning.remedy}
      </span>
    </div>
  );
}
