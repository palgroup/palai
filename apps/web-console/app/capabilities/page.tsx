"use client";

import { NameCell, Panel } from "@/components/Panel";

interface CapabilityRow extends Record<string, unknown> {
  name: string;
  tier: string;
}

interface PostureRow extends Record<string, unknown> {
  property: string;
  label: string;
  value: string;
}

// O9 — WHAT THIS DEPLOYMENT SERVES, AND AT WHICH TIER (feature list §7 O9, plan §T8). GET /v1/capabilities
// is the discovery surface every client reads to learn what a deployment supports without probing each
// route, and `palai up` has printed it to a terminal since E22 (up.go capabilityRows).
//
// THE TIERS ARE SHOWN EXACTLY AS THE API RETURNS THEM, and this is the one screen in the console where that
// is a hard constraint rather than a preference. `a2a`, `slack`, `queues`, `knowledge` and
// `capability-workers` are advertised ONLY where the binary MOUNTED them (api/capabilities.go), so the SET
// differs between deployments by design and a console rendering a fixed list would hide the one a deployment
// stopped mounting. The tier WORD is not this console's to soften either: it is recomputed at the exit gate
// from per-case outcomes (uat.CapabilityTierProof) and served bit-equal to that recompute.
//
// SO THE TIER CELL IS A CHIP AND NOT A STATUS PILL, deliberately. components/Status.tsx renders a GLYPH plus
// the word, and tests/observability.spec.ts reads this table's second cell with `textContent` and compares it
// to the served matrix — a glyph in that cell is a console disagreeing with the gate that decided the tier.
// The chip carries the word and nothing else.
// TODO(component-layer): Badge — this chip wants a per-tier colour band. `data-tier` is written on it and
// app/globals.css carries no rule for that attribute today, so the five tiers currently look alike.
//
// AND THERE IS NO THIRD COLUMN, WHICH WAS A MEASUREMENT RATHER THAN RESTRAINT. A "family" column grouping
// `slack.events` under `slack` was drafted and then deleted: `grep -n 'caps\[\|":' apps/control-plane/api/
// capabilities.go` (2026-07-31) lists eleven advertised names — responses, sessions, workspaces,
// knowledge-vector, apple-build, console, a2a, capability-workers, slack, knowledge, queues — and NOT ONE of
// them contains a dot. The column would have repeated the name in every row of every deployment. A table
// gains columns from the data, not from a wish for more of them.
export default function CapabilitiesPage() {
  return (
    <>
      <Panel<PostureRow>
        title="Deployment posture"
        testId="panel-deployment-posture"
        fetchPath="/capabilities"
        selectRows={(body) => [
          { property: "maturity", label: "Maturity", value: String(body.maturity ?? "—") },
          { property: "isolation", label: "Isolation", value: String(body.isolation ?? "—") },
          { property: "retention.store_false_ttl_seconds", label: "store:false retention", value: retentionText(body.retention) },
        ]}
        columns={[
          {
            header: "Property",
            render: (row) => <NameCell name={row.label} id={row.property} />,
          },
          { header: "Value", render: (row) => row.value },
        ]}
        emptyNote={
          <>
            <p className="empty-title">No posture was declared</p>
            <p className="empty-body">
              GET /v1/capabilities answered without a maturity or an isolation. Every client reads those two
              fields to decide what this deployment may be trusted with, so their absence is a fault in the
              deployment rather than in this screen.
            </p>
          </>
        }
      />

      <Panel<CapabilityRow>
        title="Capabilities"
        testId="panel-capabilities"
        fetchPath="/capabilities"
        // ONE SENTENCE, AND IT IS THE ONE A COLUMN CANNOT CARRY: what an ABSENT row means. A reader can see
        // the tiers; they cannot see that the list is a report of what was mounted rather than a fixed menu.
        note="A capability this binary did not mount is ABSENT here rather than shown as unavailable — discovery never claims what a deployment cannot serve."
        selectRows={(body) => {
          const caps = body.capabilities;
          if (typeof caps !== "object" || caps === null) return [];
          // Sorted by NAME so the table is stable between reads. The tier string itself is untouched.
          return Object.entries(caps as Record<string, unknown>)
            .map(([name, tier]) => ({ name, tier: String(tier) }))
            .sort((a, b) => a.name.localeCompare(b.name));
        }}
        columns={[
          // FIRST CELL: THE NAME, ALONE. tests/observability.spec.ts reads `cells[0].textContent` and diffs
          // it against the served matrix — anything else in this cell is a name the API never advertised.
          { header: "Capability", sort: (row) => row.name, render: (row) => <code>{row.name}</code> },
          // SECOND CELL: THE TIER, ALONE, for the same reason. See the header.
          {
            header: "Tier",
            sort: (row) => row.tier,
            render: (row) => (
              <span className="chip" data-tier={row.tier}>
                {row.tier}
              </span>
            ),
          },
        ]}
        filterLabel="Filter capabilities by name or tier"
        filterPlaceholder="Capability or tier…"
        matchOn={(row) => `${row.name} ${row.tier}`}
        emptyNote={
          <>
            <p className="empty-title">This deployment advertises nothing</p>
            <p className="empty-body">
              Discovery answered with an empty matrix, so a client reading it would conclude there is nothing
              here to call. That is a statement about the binary that is running, not about this screen.
            </p>
          </>
        }
      />
    </>
  );
}

// retentionText renders the configured store:false TTL. Zero is the DISABLED posture (api/capabilities.go
// configuredRetentionTTL returns 0 for an unset or unparseable value and the reaper honours the same knob),
// and "0" alone would read as a TTL of zero seconds — the opposite meaning.
function retentionText(retention: unknown): string {
  if (typeof retention !== "object" || retention === null) return "—";
  const seconds = (retention as { store_false_ttl_seconds?: unknown }).store_false_ttl_seconds;
  if (typeof seconds !== "number") return "—";
  return seconds === 0 ? "0 seconds — no store:false reaping is configured" : `${String(seconds)} seconds`;
}
