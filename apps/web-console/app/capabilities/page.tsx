"use client";

import { Panel } from "@/components/Panel";

interface CapabilityRow extends Record<string, unknown> {
  name: string;
  tier: string;
}

interface PostureRow extends Record<string, unknown> {
  property: string;
  value: string;
}

// O9 — WHAT THIS DEPLOYMENT SERVES, AND AT WHICH TIER (feature list §7 O9, plan §T8). GET /v1/capabilities
// is the discovery surface every client reads to learn what a deployment supports without probing each
// route, and `palai up` has printed it to a terminal since E22 (up.go capabilityRows) — while the console,
// the thing an operator actually looks at, did not show it at all.
//
// THE TIERS ARE SHOWN EXACTLY AS THE API RETURNS THEM. No mapping, no re-wording, no console-side opinion
// about what "preview" ought to mean, and no filtering of a capability this console does not recognise:
// `a2a`, `slack`, `queues`, `knowledge` and `capability-workers` are advertised ONLY where the binary
// MOUNTED them (api/capabilities.go), so the SET differs between deployments by design, and a console that
// rendered a fixed list of names would silently hide the one that a deployment stopped mounting. The tier
// word is also not this console's to soften: it is recomputed at the exit gate from per-case outcomes
// (uat.CapabilityTierProof) and served bit-equal to that recompute, so a prettified word here would be a
// console disagreeing with the gate that decided it.
//
// ponytail: two Panels over ONE route rather than one Panel plus a bespoke header. Both read
// /v1/capabilities, so this page fetches it twice — measured and accepted, exactly as the admin surface
// already fetches /agents twice (pagination.spec.ts records that). What it buys is that the posture gets
// the same loading / empty / error states as everything else on this console rather than a third hand-rolled
// set of them.
export default function CapabilitiesPage() {
  return (
    <>
      <Panel<PostureRow>
        title="Deployment posture"
        testId="panel-deployment-posture"
        fetchPath="/capabilities"
        note="The LP-0 declaration this deployment publishes to every client — the same body the capability table below reads."
        selectRows={(body) => [
          { property: "maturity", value: String(body.maturity ?? "—") },
          { property: "isolation", value: String(body.isolation ?? "—") },
          {
            property: "store:false retention TTL",
            value: retentionText(body.retention),
          },
        ]}
        columns={[
          { header: "Property", render: (row) => <code>{row.property}</code> },
          { header: "Value", render: (row) => row.value },
        ]}
      />

      <Panel<CapabilityRow>
        title="Capabilities"
        testId="panel-capabilities"
        fetchPath="/capabilities"
        note={
          <>
            Every capability this deployment advertises, with the tier it advertises it at — verbatim. A
            capability whose backing surface this binary did not mount is <strong>absent</strong> from the
            list rather than shown as unavailable, because discovery never claims what the deployment cannot
            serve.
          </>
        }
        selectRows={(body) => {
          const caps = body.capabilities;
          if (typeof caps !== "object" || caps === null) return [];
          // Sorted by NAME so the table is stable between reads. The tier string itself is untouched.
          return Object.entries(caps as Record<string, unknown>)
            .map(([name, tier]) => ({ name, tier: String(tier) }))
            .sort((a, b) => a.name.localeCompare(b.name));
        }}
        columns={[
          { header: "Capability", render: (row) => <code>{row.name}</code> },
          { header: "Tier", render: (row) => row.tier },
        ]}
        emptyNote="This deployment advertises no capabilities at all, which means discovery answered with an empty matrix — a client reading it would conclude there is nothing here to call."
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
