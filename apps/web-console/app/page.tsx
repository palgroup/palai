"use client";

import { AgentDiff } from "@/components/AgentDiff";
import { Panel } from "@/components/Panel";

// The §47.1 admin surface. Every panel fetches a /v1 list through the same-origin relay (browser →
// /api/palai/v1/* → upstream /v1/*), so the public-API-only intercept sees the whole admin surface ride
// the relay. Secret-ref VALUES are never rendered — the read surface is metadata only.
export default function AdminPage() {
  return (
    <>
      <p className="muted" data-testid="ceiling-note">
        Open-core console — <strong>public API only</strong>. This is not a commercial SaaS UI: no billing
        or team management (§5). The operator console (cordon/drain/queue-inspect, §47.4) lives in the E15
        ops CLIs, and config explainability shows the effective value only (layer-by-layer attribution is a
        later iteration, §47.3).
      </p>

      <Panel
        title="Organizations"
        testId="panel-organizations"
        fetchPath="/organizations"
        columns={[
          { header: "ID", render: (r) => String(r.id ?? "") },
          { header: "Name", render: (r) => String(r.display_name ?? "") },
        ]}
      />

      <Panel
        title="Projects"
        testId="panel-projects"
        fetchPath="/projects"
        columns={[
          { header: "ID", render: (r) => String(r.id ?? "") },
          { header: "Name", render: (r) => String(r.display_name ?? "") },
          { header: "Organization", render: (r) => String(r.organization_id ?? "") },
        ]}
      />

      <Panel
        title="API keys"
        testId="panel-api-keys"
        fetchPath="/api-keys"
        note="Key plaintext is disclosed once at creation and never on a read — this surface shows metadata only."
        columns={[
          { header: "ID", render: (r) => String(r.id ?? "") },
          { header: "Project", render: (r) => String(r.project_id ?? "") },
          { header: "Scopes", render: (r) => (Array.isArray(r.scopes) ? r.scopes.join(", ") : "") },
          { header: "Status", render: (r) => (r.revoked_at ? "revoked" : "active") },
        ]}
      />

      <Panel
        title="Model connections"
        testId="panel-model-connections"
        fetchPath="/model-connections"
        note="A connection binds a provider to a secret REF (a name), never a value."
        columns={[
          { header: "ID", render: (r) => String(r.id ?? "") },
          { header: "Provider", render: (r) => String(r.provider ?? "") },
          { header: "Secret ref", render: (r) => String(r.secret_ref ?? "") },
        ]}
      />

      <Panel
        title="Model routes"
        testId="panel-model-routes"
        fetchPath="/model-routes"
        note="E16 T1 read-back of the E13 write-only route surface."
        columns={[
          { header: "ID", render: (r) => String(r.id ?? "") },
          { header: "Name", render: (r) => String(r.name ?? "") },
        ]}
      />

      <Panel
        title="Secret refs"
        testId="panel-secret-refs"
        fetchPath="/secret-refs"
        note="METADATA ONLY. A secret VALUE is write-only server-side and never appears in any response."
        columns={[
          { header: "Name", render: (r) => String(r.name ?? "") },
          { header: "Version", render: (r) => String(r.version ?? "") },
        ]}
      />

      <Panel
        title="Knowledge bases"
        testId="panel-knowledge-bases"
        fetchPath="/knowledge-bases"
        columns={[
          { header: "ID", render: (r) => String(r.id ?? "") },
          { header: "Name", render: (r) => String(r.display_name ?? r.name ?? "") },
        ]}
      />

      {/* The one PAGINATED collection on this surface: api/pagination.go serves agents through renderPage, so
          this panel is where truncation is visible rather than assumed (E25 T2, plan §3.6 D18). */}
      <Panel
        title="Agents"
        testId="panel-agents"
        fetchPath="/agents"
        columns={[{ header: "ID", render: (r) => String(r.id ?? "") }]}
      />

      <AgentDiff />
    </>
  );
}
