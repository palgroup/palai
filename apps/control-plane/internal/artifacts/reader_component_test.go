//go:build component

package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// TestReaderMetadataAndContent proves the retrieval read-path against real infrastructure: an artifact
// written by the write-path is read back through the Reader's metadata projection and its streaming content
// download, and the streamed bytes' SHA-256 is bit-identical to the checksum the write-path recorded — the
// integrity guarantee the download's Content-Digest header carries (spec §22.6, E13 T5).
func TestReaderMetadataAndContent(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	org, project, runID := h.seedRun(t)
	reader := NewReader(h.s3, h.pool)
	scope := middleware.Scope{Organization: org, Project: project}

	content := []byte("diff --git a/main.go b/main.go\n+ // fixed\n")
	art, err := h.writer.Write(ctx, WriteRequest{
		Organization: org, Project: project, RunID: runID, Content: content,
		MediaType: "text/x-diff", LogicalType: "patch",
	})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	// Metadata: the JSON projection carries identity, the run, integrity, and §22.6 classification —
	// and never the object key (the S3 layout is control-plane internal, spec §24).
	metaRes, err := reader.GetArtifact(ctx, scope, art.ID)
	if err != nil {
		t.Fatalf("GetArtifact() error = %v", err)
	}
	if metaRes.NotFound {
		t.Fatal("GetArtifact reported NotFound for an artifact the write-path just committed")
	}
	var meta map[string]any
	if err := json.Unmarshal(metaRes.Body, &meta); err != nil {
		t.Fatalf("metadata is not valid JSON: %v", err)
	}
	if meta["id"] != art.ID || meta["run_id"] != runID {
		t.Fatalf("metadata id/run = %v/%v, want %s/%s", meta["id"], meta["run_id"], art.ID, runID)
	}
	if meta["checksum"] != art.Checksum || meta["media_type"] != "text/x-diff" || meta["logical_type"] != "patch" {
		t.Fatalf("metadata classification/integrity = %v, want checksum=%s media=text/x-diff logical=patch", meta, art.Checksum)
	}
	if meta["malware_scan_status"] != "not_scanned" {
		t.Fatalf("metadata malware_scan_status = %v, want not_scanned (honest — no scanner wired)", meta["malware_scan_status"])
	}
	if _, leaked := meta["object_key"]; leaked {
		t.Fatal("metadata leaked the internal object_key")
	}

	// Content: the streaming download's bytes are bit-identical to what was written, and its
	// Content-Digest is the RFC 9530 rendering of the same SHA-256.
	cnt, err := reader.OpenArtifactContent(ctx, scope, art.ID)
	if err != nil {
		t.Fatalf("OpenArtifactContent() error = %v", err)
	}
	if cnt.NotFound {
		t.Fatal("OpenArtifactContent reported NotFound for a live artifact")
	}
	got, err := io.ReadAll(cnt.Reader)
	_ = cnt.Reader.Close()
	if err != nil {
		t.Fatalf("read streamed content: %v", err)
	}
	sum := sha256.Sum256(got)
	if "sha256:"+hex.EncodeToString(sum[:]) != art.Checksum {
		t.Fatal("streamed bytes are NOT bit-identical to the written artifact (checksum mismatch)")
	}
	if cnt.SizeBytes != int64(len(content)) {
		t.Fatalf("content size = %d, want %d", cnt.SizeBytes, len(content))
	}
	wantDigest := "sha-256=:" + base64.StdEncoding.EncodeToString(sum[:]) + ":"
	if cnt.Digest != wantDigest {
		t.Fatalf("Content-Digest = %q, want %q", cnt.Digest, wantDigest)
	}
}

// TestReaderCrossTenantIsMiss is DAT-006's basic half at the read-path: an artifact written under one
// tenant is invisible to another — both metadata and content read as a plain NotFound (the 404 the API
// renders), never a signal that the artifact exists elsewhere. The DB read (RLS + WHERE) is the gate, so
// the foreign request never reaches the object store.
func TestReaderCrossTenantIsMiss(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	orgA, projectA, runA := h.seedRun(t)
	orgB, projectB, _ := h.seedRun(t)
	reader := NewReader(h.s3, h.pool)

	art, err := h.writer.Write(ctx, WriteRequest{Organization: orgA, Project: projectA, RunID: runA, Content: []byte("secret build log")})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	foreign := middleware.Scope{Organization: orgB, Project: projectB}
	metaRes, err := reader.GetArtifact(ctx, foreign, art.ID)
	if err != nil {
		t.Fatalf("cross-tenant GetArtifact() error = %v", err)
	}
	if !metaRes.NotFound || metaRes.Body != nil {
		t.Fatal("a foreign tenant saw artifact metadata; cross-tenant existence leaked")
	}
	cnt, err := reader.OpenArtifactContent(ctx, foreign, art.ID)
	if err != nil {
		t.Fatalf("cross-tenant OpenArtifactContent() error = %v", err)
	}
	if !cnt.NotFound || cnt.Reader != nil {
		if cnt.Reader != nil {
			_ = cnt.Reader.Close()
		}
		t.Fatal("a foreign tenant opened artifact content; the object store was reached across the boundary")
	}
}

// TestReaderListRunArtifacts proves the run-scoped list: a response's run surfaces exactly its own
// artifacts, and an unknown or foreign response id is a non-disclosing NotFound (never an empty 200 that
// would confirm the response's existence is being probed).
func TestReaderListRunArtifacts(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	org, project, responseID, runID := h.seedResponseRun(t)
	reader := NewReader(h.s3, h.pool)
	scope := middleware.Scope{Organization: org, Project: project}

	for _, c := range [][]byte{[]byte("patch bytes"), []byte("test log bytes")} {
		if _, err := h.writer.Write(ctx, WriteRequest{Organization: org, Project: project, RunID: runID, Content: c}); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
	}

	res, err := reader.ListRunArtifacts(ctx, scope, responseID)
	if err != nil {
		t.Fatalf("ListRunArtifacts() error = %v", err)
	}
	if res.NotFound {
		t.Fatal("ListRunArtifacts reported NotFound for the tenant's own response")
	}
	var list struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(res.Body, &list); err != nil {
		t.Fatalf("list is not valid JSON: %v", err)
	}
	if list.Object != "list" || len(list.Data) != 2 {
		t.Fatalf("list = %+v, want object=list with 2 artifacts", list)
	}

	// An unknown response id is a non-disclosing miss, not an empty list.
	if miss, err := reader.ListRunArtifacts(ctx, scope, "resp_does_not_exist"); err != nil || !miss.NotFound {
		t.Fatalf("unknown response list = %+v (err %v), want NotFound", miss, err)
	}
}

// seedResponseRun creates org -> project -> session -> response -> run (with response_id set), so the
// response->run resolution the run-scoped list relies on has a row to find.
func (h *artifactsHarness) seedResponseRun(t *testing.T) (org, project, responseID, runID string) {
	t.Helper()
	org, project = newID("org"), newID("prj")
	session := newID("ses")
	responseID = newID("resp")
	runID = newID("run")
	h.exec(t, `INSERT INTO organizations (id) VALUES ($1)`, org)
	h.exec(t, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, project, org)
	h.exec(t, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`, session, org, project)
	h.exec(t, `INSERT INTO responses (id, organization_id, project_id, session_id, input) VALUES ($1, $2, $3, $4, '{}'::jsonb)`, responseID, org, project, session)
	h.exec(t, `INSERT INTO runs (id, organization_id, project_id, session_id, response_id) VALUES ($1, $2, $3, $4, $5)`, runID, org, project, session, responseID)
	return org, project, responseID, runID
}
