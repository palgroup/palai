// Code generated from the canonical LocalLiveEvidenceManifest schema; DO NOT EDIT.
package contracts

type Case struct {
	Checksum          string         `json:"checksum"`
	DbAssertions      []string       `json:"db_assertions"`
	ID                string         `json:"id"`
	ImageDigest       string         `json:"image_digest"`
	MtlsEnroll        string         `json:"mtls_enroll"`
	ProofClass        string         `json:"proof_class"`
	ProviderRequestID string         `json:"provider_request_id"`
	RunID             string         `json:"run_id"`
	Status            string         `json:"status"`
	Terminal          map[string]any `json:"terminal"`
	Usage             map[string]any `json:"usage"`
}

type LocalLiveEvidenceManifest struct {
	ApiVersion string           `json:"api_version"`
	CapturedAt string           `json:"captured_at"`
	Cases      []map[string]any `json:"cases"`
	GitSha     string           `json:"git_sha"`
	Migration  string           `json:"migration"`
	Release    string           `json:"release"`
}
