package knowledge

import "time"

// knowledgeBaseView is a knowledge base projection.
type knowledgeBaseView struct {
	ID                    string     `json:"id"`
	Object                string     `json:"object"`
	Name                  string     `json:"name"`
	EmbeddingRoute        string     `json:"embedding_route,omitempty"`
	ActiveIndexRevisionID string     `json:"active_index_revision_id,omitempty"`
	CreatedAt             *time.Time `json:"created_at,omitempty"`
}

func scanKnowledgeBase(row scanner) (knowledgeBaseView, error) {
	v := knowledgeBaseView{Object: "knowledge_base"}
	var active *string
	var createdAt time.Time
	if err := row.Scan(&v.ID, &v.Name, &v.EmbeddingRoute, &active, &createdAt); err != nil {
		return knowledgeBaseView{}, err
	}
	if active != nil {
		v.ActiveIndexRevisionID = *active
	}
	v.CreatedAt = &createdAt
	return v, nil
}

// sourceView is a knowledge source projection.
type sourceView struct {
	ID             string     `json:"id"`
	Object         string     `json:"object"`
	Kind           string     `json:"kind"`
	URI            string     `json:"uri"`
	ACL            string     `json:"acl,omitempty"`
	Classification string     `json:"classification,omitempty"`
	Parser         string     `json:"parser"`
	CreatedAt      *time.Time `json:"created_at,omitempty"`
}

// ingestionView is an ingestion job outcome projection.
type ingestionView struct {
	Object             string `json:"object"`
	ID                 string `json:"id"`
	State              string `json:"state"`
	DocumentRevisionID string `json:"document_revision_id,omitempty"`
	IndexRevisionID    string `json:"index_revision_id,omitempty"`
	IndexVersion       int    `json:"index_version,omitempty"`
	ChunkCount         int    `json:"chunk_count,omitempty"`
	Error              string `json:"error,omitempty"`
}

// indexRevisionView is an index revision (build snapshot) projection.
type indexRevisionView struct {
	ID                string     `json:"id"`
	Object            string     `json:"object"`
	Version           int        `json:"version"`
	State             string     `json:"state"`
	DocumentRevisions int        `json:"document_revisions"`
	ChunkCount        int        `json:"chunk_count"`
	CreatedAt         *time.Time `json:"created_at,omitempty"`
}

// RetrievedChunk is one typed retrieval hit (§25.15.5). It carries the revision + source identity, the EXACT
// byte offsets and checksum (so a citation is verifiable against the document bytes), the score(s), the
// chunk timestamp, the trust class, and a stable citation ref. TrustClass is ALWAYS "untrusted": retrieved
// source content is data in the tool-result layer, never a privileged instruction and never a capability
// grant (KNO-006) — it is stamped on every hit so a consumer cannot mistake it for authored context. ACL is
// the authorization label the query-level predicate already enforced (the T5 hardening derives the grant
// server-side). Exported because the component tests, the retrieval tool, and the API projection consume it.
type RetrievedChunk struct {
	Object             string     `json:"object"`
	ChunkID            string     `json:"chunk_id"`
	SourceID           string     `json:"source_id"`
	DocumentRevisionID string     `json:"document_revision_id"`
	IndexRevisionID    string     `json:"index_revision_id"`
	Ordinal            int        `json:"ordinal"`
	ByteStart          int        `json:"byte_start"`
	ByteEnd            int        `json:"byte_end"`
	Checksum           string     `json:"checksum"`
	ACL                string     `json:"acl,omitempty"`
	TrustClass         string     `json:"trust_class"`
	CitationRef        string     `json:"citation_ref"`
	Content            string     `json:"content"`
	Score              float64    `json:"score"`
	Strategy           string     `json:"strategy"`
	CreatedAt          *time.Time `json:"created_at,omitempty"`
}

// scanner is the row shape shared by pool.QueryRow and rows.
type scanner interface {
	Scan(dest ...any) error
}
