//go:build component

// E17 Task 5 e2e-deterministic proof: a fake-engine run uses the built-in retrieval tool and CITES, and the
// citation byte offsets re-verify against the stored document bytes (content[start:end] == the chunk). It
// drives the REAL knowledge store (the tool's Retriever seam) against a real Postgres FTS index — no real
// provider, deterministic (E08: the engine opens no tool to a real provider). The tool wraps retrieval as an
// UNTRUSTED tool result, so this also exercises the KNO-006 trust stamp end to end.
package knowledge_test

import (
	"context"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

func TestRetrievalToolCitesVerifiableOffsetsEndToEnd(t *testing.T) {
	cs, ks := openStore(t)
	scope := provisionTenant(t, cs, "kno-tool-e2e")
	kb := createKB(t, ks, scope, "kb")
	src := createSource(t, ks, scope, kb, "")
	ingest(t, ks, scope, kb, src,
		"Postgres full text search ranks documents.\n\nFull text search uses tsvector and tsquery for ranking.")

	// The fake-engine run invokes the built-in tool; ks is the server-side Retriever (grants server-derived,
	// none here -> KB-wide only). env.Scope carries the run's tenant identity.
	tool := tools.KnowledgeRetrievalTool(ks)
	env := toolbroker.ExecEnv{Scope: toolbroker.TaskScope{Org: scope.Organization, Project: scope.Project, RunID: "run_e2e"}}
	out, err := tool.Exec(context.Background(), env, map[string]any{"knowledge_base_id": kb, "query": "full text search ranking"})
	if err != nil {
		t.Fatalf("tool Exec error = %v", err)
	}
	if out["trust_class"] != "untrusted" {
		t.Fatalf("tool result not stamped untrusted: %+v", out)
	}
	citations, ok := out["citations"].([]map[string]any)
	if !ok || len(citations) == 0 {
		t.Fatalf("tool cited nothing: %+v", out["citations"])
	}
	for _, c := range citations {
		docRev, _ := c["document_revision_id"].(string)
		start, _ := c["byte_start"].(int)
		end, _ := c["byte_end"].(int)
		want, _ := c["content"].(string)
		content, err := ks.DocumentContent(context.Background(), scope, docRev)
		if err != nil {
			t.Fatalf("DocumentContent error = %v", err)
		}
		if start < 0 || end > len(content) || start > end {
			t.Fatalf("citation offsets [%d,%d) out of bounds (len %d)", start, end, len(content))
		}
		if got := content[start:end]; got != want {
			t.Fatalf("citation offsets recover %q, want cited content %q", got, want)
		}
	}
}
