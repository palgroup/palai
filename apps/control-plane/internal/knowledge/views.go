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

// RetrievedChunk is one ranked retrieval hit. It carries the stable citation coordinates (document_revision
// id + byte offsets + checksum) so a citation is verifiable against the document bytes, and the FTS score.
// ACL is included so a caller (and T5's hardening) can see the authorization label the query already
// enforced. Exported because the component tests and the T5 retrieval layer consume it directly.
type RetrievedChunk struct {
	Object             string  `json:"object"`
	ChunkID            string  `json:"chunk_id"`
	SourceID           string  `json:"source_id"`
	DocumentRevisionID string  `json:"document_revision_id"`
	Ordinal            int     `json:"ordinal"`
	ByteStart          int     `json:"byte_start"`
	ByteEnd            int     `json:"byte_end"`
	Checksum           string  `json:"checksum"`
	ACL                string  `json:"acl,omitempty"`
	Content            string  `json:"content"`
	Score              float64 `json:"score"`
}

// scanner is the row shape shared by pool.QueryRow and rows.
type scanner interface {
	Scan(dest ...any) error
}
