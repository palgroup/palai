package objectstore

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestEvaluateCandidateRejectsEveryRequiredMissingReceipt(t *testing.T) {
	valid := completeCandidate()
	tests := map[string]func(*Candidate){
		"primary_source.url":        func(candidate *Candidate) { candidate.PrimarySource.URL = "" },
		"primary_source.commit":     func(candidate *Candidate) { candidate.PrimarySource.Commit = "" },
		"license.spdx":              func(candidate *Candidate) { candidate.License.SPDX = "" },
		"license.receipt_url":       func(candidate *Candidate) { candidate.License.ReceiptURL = "" },
		"maintenance.active":        func(candidate *Candidate) { candidate.Maintenance.Active = nil },
		"maintenance.checked_at":    func(candidate *Candidate) { candidate.Maintenance.CheckedAt = "" },
		"maintenance.status":        func(candidate *Candidate) { candidate.Maintenance.Status = "" },
		"maintenance.receipt_url":   func(candidate *Candidate) { candidate.Maintenance.ReceiptURL = "" },
		"image.index_digest":        func(candidate *Candidate) { candidate.Image.IndexDigest = "" },
		"image.linux_amd64":         func(candidate *Candidate) { candidate.Image.Platforms = candidate.Image.Platforms[1:] },
		"image.linux_arm64":         func(candidate *Candidate) { candidate.Image.Platforms = candidate.Image.Platforms[:1] },
		"readiness.command":         func(candidate *Candidate) { candidate.Readiness.Command = "" },
		"readiness.semantics":       func(candidate *Candidate) { candidate.Readiness.Semantics = "" },
		"s3_conformance.command":    func(candidate *Candidate) { candidate.S3Conformance.Command = "" },
		"offline.export_command":    func(candidate *Candidate) { candidate.Offline.ExportCommand = "" },
		"offline.import_command":    func(candidate *Candidate) { candidate.Offline.ImportCommand = "" },
		"offline.claim":             func(candidate *Candidate) { candidate.Offline.Claim = "" },
		"supply_chain.checked_at":   func(candidate *Candidate) { candidate.SupplyChain.CheckedAt = "" },
		"supply_chain.status":       func(candidate *Candidate) { candidate.SupplyChain.Status = "" },
		"supply_chain.check_method": func(candidate *Candidate) { candidate.SupplyChain.CheckMethod = "" },
	}

	for expectedIssue, mutate := range tests {
		t.Run(expectedIssue, func(t *testing.T) {
			candidate := valid
			candidate.Image.Platforms = append([]PlatformDigest(nil), valid.Image.Platforms...)
			mutate(&candidate)

			result := EvaluateCandidate(candidate)
			if result.Complete {
				t.Fatal("candidate with a missing receipt was complete")
			}
			if !containsIssue(result.Issues, expectedIssue) {
				t.Fatalf("issues %q do not identify %q", result.Issues, expectedIssue)
			}
		})
	}
}

func TestEvaluateCandidateKeepsCompleteNegativeEvidenceDistinctFromMissingEvidence(t *testing.T) {
	candidate := completeCandidate()
	candidate.ID = "minio-community"
	candidate.Maintenance.Active = boolPointer(false)
	candidate.Maintenance.Status = "archived; latest community release is source-only"
	candidate.Distribution.Status = "source-only"

	result := EvaluateCandidate(candidate)
	if !result.Complete {
		t.Fatalf("complete negative record was structurally rejected: %v", result.Issues)
	}
	if result.Eligible {
		t.Fatal("archived, source-only candidate was eligible")
	}
}

func TestEvaluateCandidateDoesNotTurnAGPLReviewConcernIntoLegalConclusion(t *testing.T) {
	candidate := completeCandidate()
	candidate.ID = "garage"
	candidate.License.SPDX = "AGPL-3.0-only"
	candidate.Distribution.PolicyConcern = "distribution policy review required"

	result := EvaluateCandidate(candidate)
	if !result.Complete || !result.Eligible {
		t.Fatalf("active complete candidate was rejected as a legal conclusion: %+v", result)
	}
}

func TestLoadAndEvaluateCatalogSelectsSeaweedFSByImmutableIndex(t *testing.T) {
	file, err := os.Open("candidates.json")
	if err != nil {
		t.Fatalf("open candidate catalog: %v", err)
	}
	defer file.Close()

	catalog, err := DecodeCatalog(file)
	if err != nil {
		t.Fatalf("decode candidate catalog: %v", err)
	}
	evaluation, err := EvaluateCatalog(catalog)
	if err != nil {
		t.Fatalf("evaluate candidate catalog: %v", err)
	}
	const selected = "docker.io/chrislusf/seaweedfs@sha256:c7d6c721b30ae711db766bbbfd40192776e263d4e51e22f57baef7bef93c12c6"
	if evaluation.Selected.ID != "seaweedfs" || catalog.Selection.ImmutableReference != selected {
		t.Fatalf("selection = %q %q", evaluation.Selected.ID, catalog.Selection.ImmutableReference)
	}
	if !strings.Contains(catalog.Selection.ReleaseRequirement, "mirror") ||
		!strings.Contains(catalog.Selection.ReleaseRequirement, "sign") ||
		!strings.Contains(catalog.Selection.ReleaseRequirement, "E18") {
		t.Fatal("selection omits the E18 mirror/sign requirement")
	}
	if len(evaluation.Candidates) != 3 {
		t.Fatalf("candidate count = %d, want 3", len(evaluation.Candidates))
	}
}

func TestDecodeCatalogRejectsUnknownFields(t *testing.T) {
	data, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"researched_at":  "2026-07-16",
		"selection":      map[string]any{},
		"candidates":     []any{},
		"invented":       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCatalog(strings.NewReader(string(data))); err == nil {
		t.Fatal("unknown candidate catalog field was accepted")
	}
}

func completeCandidate() Candidate {
	return Candidate{
		ID:      "complete",
		Name:    "Complete Candidate",
		Version: "v1.0.0",
		PrimarySource: PrimarySourceEvidence{
			URL:    "https://example.invalid/releases/v1.0.0",
			Commit: strings.Repeat("a", 40),
		},
		License: LicenseEvidence{
			SPDX:       "Apache-2.0",
			ReceiptURL: "https://example.invalid/LICENSE",
		},
		Maintenance: MaintenanceEvidence{
			Active:     boolPointer(true),
			CheckedAt:  "2026-07-16",
			Status:     "active release",
			ReceiptURL: "https://example.invalid/releases",
		},
		Image: ImageEvidence{
			TagReference: "docker.io/example/project:v1.0.0",
			IndexDigest:  "sha256:" + strings.Repeat("b", 64),
			Platforms: []PlatformDigest{
				{OS: "linux", Architecture: "amd64", Digest: "sha256:" + strings.Repeat("c", 64)},
				{OS: "linux", Architecture: "arm64", Digest: "sha256:" + strings.Repeat("d", 64)},
			},
		},
		Readiness: ReadinessEvidence{
			Command:   "GET /health",
			Semantics: "200 ready; 503 unavailable",
		},
		S3Conformance: CommandEvidence{Command: "go test -run TestS3Conformance"},
		Offline: OfflineEvidence{
			ExportCommand: "docker image save",
			ImportCommand: "docker image load",
			Claim:         "local archive round-trip",
		},
		SupplyChain: SupplyChainEvidence{
			CheckedAt:   "2026-07-16",
			Status:      "not discoverable by scoped registry checks",
			CheckMethod: "OCI referrers and recognized Cosign legacy artifact conventions",
		},
		Distribution: DistributionEvidence{Status: "container-distributed"},
	}
}

func containsIssue(issues []string, expected string) bool {
	for _, issue := range issues {
		if issue == expected {
			return true
		}
	}
	return false
}

func boolPointer(value bool) *bool {
	return &value
}
