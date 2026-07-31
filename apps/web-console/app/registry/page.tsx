"use client";

import { NameCell, Panel, type Column } from "@/components/Panel";
import { CopyButton, shortId, Stamp } from "@/components/Session";

// MODEL WIRING — three registries, and until this pass they were three lists pretending to be tables.
//
// WHAT WAS MEASURED (2026-07-31, `PROSE MEASURE /registry`): three tables carrying FOUR header cells between
// them — Provider + Secret ref on the first, and ONE column each on the other two. A one-column table is a
// list with a border round it: it cannot be sorted, it cannot be scanned down, and the id it does carry was
// a 36-character hash printed whole. Alongside them, 593 characters of paragraph over 773 characters of
// page — more prose than content.
//
// WHAT IT IS NOW: an id cell that is readable AND copyable, the one or two attributes that identify the row,
// and the stamp that says how old it is. Eleven header cells over the same three tables.
//
// NOTHING ON THIS SCREEN WRITES, and that is the API's shape rather than a simplification: /v1 mounts no
// console-reachable create for a model connection, a model route or a knowledge base. Each empty state says
// so where an operator meets it, rather than a page-level paragraph saying it three times over.
//
// `created_at` IS ON ALL THREE REAL PROJECTIONS and is rendered from all three (internal/store/
// model_routes.go modelConnectionView/modelRouteView write it unconditionally; knowledge/views.go's
// knowledgeBaseView carries it too). The deterministic fixture carries it on the knowledge base alone, so on
// the fake profile the other two render the em dash Stamp gives an absent time — which is the honest cell
// for a row whose stamp did not arrive, and not a defect in this screen.

/** IdCell is this console's row identity: a readable prefix that links nowhere, and the whole value one
 *  click away. Ours are `<prefix>_` + a hash, and nobody reads the middle of a hash — what a reader needs is
 *  to recognise the row again and to be able to copy the value EXACTLY. There is no detail route for any of
 *  these three collections (no /v1 read of a single connection, route or base is mounted), so the id is not
 *  a link here: a link to a page that cannot exist is worse than a value that can be copied. */
function IdCell({ id, label }: { id: string; label: string }) {
  if (id === "") return <span className="cell-none">— none</span>;
  return (
    <span className="cell-id-group">
      <code className="cell-id" title={id}>
        {shortId(id)}
      </code>
      <CopyButton value={id} label={label} testId={`copy-${id}`} />
    </span>
  );
}

/** created is the column all three share, declared once so the three tables cannot describe it differently. */
const created: Column<Record<string, unknown>> = {
  header: "Created",
  sort: (r) => String(r.created_at ?? ""),
  render: (r) => <Stamp iso={typeof r.created_at === "string" ? r.created_at : null} />,
};

export default function RegistryPage() {
  return (
    <>
      <Panel
        title="Model connections"
        testId="panel-model-connections"
        fetchPath="/model-connections"
        // THE ONE SENTENCE THAT SURVIVES AS A NOTE, because it is the only claim on this screen a reader
        // cannot infer from a cell: the Secret ref column holds a NAME, and no route anywhere reads the value
        // it points at back. Everything else this page used to say above its tables is either in a column
        // header or in the empty state that needs it.
        note="A connection binds a provider to a secret REF — a name. The value behind it is written server-side and is readable through no route, this console included."
        columns={[
          { header: "ID", sort: (r) => String(r.id ?? ""), render: (r) => <IdCell id={String(r.id ?? "")} label="connection ID" /> },
          { header: "Provider", sort: (r) => String(r.provider ?? ""), render: (r) => <NameCell name={String(r.provider ?? "")} id="" /> },
          {
            // THE REF, NOT THE VALUE, and the column header is where that is said now — it used to be a
            // sentence above the table repeating what the cell already shows.
            header: "Secret ref",
            sort: (r) => String(r.secret_ref ?? ""),
            render: (r) => (String(r.secret_ref ?? "") === "" ? <span className="cell-none">— none</span> : <code>{String(r.secret_ref)}</code>),
          },
          created,
        ]}
        emptyNote={
          <>
            <p className="empty-title">No model connections yet</p>
            <p className="empty-body">
              A connection is how this deployment reaches a provider: it binds a provider name to a secret
              REF, never to a value. Until one exists every run falls back to the deployment default, and a
              project route naming a provider resolves to nothing. One is created by the provisioning API or
              the CLI — there is no console control for it.
            </p>
          </>
        }
      />

      <Panel
        title="Model routes"
        testId="panel-model-routes"
        fetchPath="/model-routes"
        columns={[
          { header: "ID", sort: (r) => String(r.id ?? ""), render: (r) => <IdCell id={String(r.id ?? "")} label="route ID" /> },
          {
            header: "Name",
            sort: (r) => String(r.name ?? ""),
            render: (r) => <NameCell name={String(r.name ?? "")} id="" />,
          },
          created,
        ]}
        emptyNote={
          <>
            <p className="empty-title">No model routes yet</p>
            <p className="empty-body">
              A route is a name a run can ask for instead of a model. With none, runs use the deployment
              default; a per-project override is created through the provisioning API, and this screen is
              where it becomes visible.
            </p>
          </>
        }
      />

      <Panel
        title="Knowledge bases"
        testId="panel-knowledge-bases"
        fetchPath="/knowledge-bases"
        columns={[
          { header: "ID", sort: (r) => String(r.id ?? ""), render: (r) => <IdCell id={String(r.id ?? "")} label="knowledge base ID" /> },
          {
            header: "Name",
            // `name`, NOT `display_name`. The real projection is knowledge/views.go's knowledgeBaseView and it
            // carries `name`; the fixture said `display_name` until E25 T2 and nothing caught it, because a
            // bootstrap stack's collection is empty and the conformance sweep skips an item comparison when
            // either side has no row.
            sort: (r) => String(r.name ?? ""),
            render: (r) => <NameCell name={String(r.name ?? "")} id="" />,
          },
          created,
        ]}
        emptyNote={
          <>
            <p className="empty-title">No knowledge bases yet</p>
            <p className="empty-body">
              A knowledge base is what a retrieval tool searches. Having none is not an error — most
              deployments never create one — and a base is created and indexed outside this console.
            </p>
          </>
        }
      />
    </>
  );
}
