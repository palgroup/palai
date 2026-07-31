"use client";

import { NameCell, Panel } from "@/components/Panel";

// MODEL WIRING — the three registries that came off the landing page (console design pass).
//
// These are read-only projections of how a model is reached: which provider this deployment can talk to,
// which secret REF holds the credential for it, which named routes select one, and which knowledge bases a
// run can retrieve from. They were panels four, five and seven of a nine-panel scroll on "/", which is where
// a deployment's least-consulted configuration should not be — an operator reads them when wiring a provider
// and then not again for weeks.
//
// NOTHING ON THIS SCREEN WRITES, and that is the API's shape rather than a simplification: /v1 mounts no
// console-reachable create for a model connection, a model route or a knowledge base. Saying so is the point
// of the page's own note — a read screen that looks like a form screen with the forms missing is a screen an
// operator waits on.
export default function RegistryPage() {
  return (
    <>
      <Panel
        title="Model connections"
        testId="panel-model-connections"
        fetchPath="/model-connections"
        note="A connection binds a provider to a secret REF (a name), never a value. The value is written server-side and can never be read back through this console."
        emptyNote="No model connection. Until one exists every run falls back to the deployment default, and a project route that names a provider resolves to nothing."
        columns={[
          {
            header: "Provider",
            sort: (r) => String(r.provider ?? ""),
            render: (r) => <NameCell name={String(r.provider ?? "")} id={String(r.id ?? "")} />,
          },
          { header: "Secret ref", sort: (r) => String(r.secret_ref ?? ""), render: (r) => <code>{String(r.secret_ref ?? "")}</code> },
        ]}
      />

      <Panel
        title="Model routes"
        testId="panel-model-routes"
        fetchPath="/model-routes"
        note="E16 T1 read-back of the E13 write-only route surface — a route is created by the provisioning API, and this is where it becomes visible."
        emptyNote="No model route. Runs use the deployment default; a per-project override is created through the provisioning API."
        columns={[
          {
            header: "Name",
            sort: (r) => String(r.name ?? r.id ?? ""),
            render: (r) => <NameCell name={String(r.name ?? "")} id={String(r.id ?? "")} />,
          },
        ]}
      />

      <Panel
        title="Knowledge bases"
        testId="panel-knowledge-bases"
        fetchPath="/knowledge-bases"
        note="What a retrieval tool can search. A base is created and indexed outside this console."
        emptyNote="No knowledge base. A retrieval tool has nothing to search, which is not an error — most deployments never create one."
        columns={[
          {
            header: "Name",
            // `name`, NOT `display_name`. The real projection is knowledge/views.go's knowledgeBaseView and it
            // carries `name`; the fixture said `display_name` until E25 T2 and nothing caught it, because a
            // bootstrap stack's collection is empty and the conformance sweep skips an item comparison when
            // either side has no row.
            sort: (r) => String(r.name ?? r.id ?? ""),
            render: (r) => <NameCell name={String(r.name ?? "")} id={String(r.id ?? "")} />,
          },
        ]}
      />
    </>
  );
}
