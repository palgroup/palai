package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	objectstore "github.com/palgroup/palai/spikes/object-store"
)

var conformanceCases = []string{
	"auth.wrong_secret_rejected",
	"bucket.create_and_head",
	"checksum.put_head_get",
	"conditional.if_none_match",
	"range.exact_bytes",
	"multipart.complete",
	"multipart.abort",
	"object.delete_not_found",
	"persistence.seeded",
}

var persistenceCases = []string{
	"persistence.retained_bytes_checksum",
	"persistence.cleanup",
}

func TestReadMeasurementsRequiresExactPhaseBoundEvidence(t *testing.T) {
	commit := strings.Repeat("a", 40)
	tree := strings.Repeat("b", 40)
	directory := t.TempDir()
	writeRunSummary(t, directory, commit, tree, 1, "conformance", conformanceCases)
	writeRunSummary(t, directory, commit, tree, 1, "persistence", persistenceCases)

	values, err := readMeasurements(directory, 1, commit, tree)
	if err != nil {
		t.Fatalf("read valid measurements: %v", err)
	}
	if values.repetitions != 1 || values.bytesVerified != 8192 || len(values.caseLatenciesMS) != 11 {
		t.Fatalf("unexpected measurements: %+v", values)
	}
}

func TestReadMeasurementsRejectsMissingMutatedAndStaleEvidence(t *testing.T) {
	commit := strings.Repeat("a", 40)
	tree := strings.Repeat("b", 40)
	tests := map[string]func(string){
		"missing phase": func(directory string) {
			writeRunSummary(t, directory, commit, tree, 1, "conformance", conformanceCases)
		},
		"missing case": func(directory string) {
			writeRunSummary(t, directory, commit, tree, 1, "conformance", conformanceCases[:len(conformanceCases)-1])
			writeRunSummary(t, directory, commit, tree, 1, "persistence", persistenceCases)
		},
		"wrong commit": func(directory string) {
			writeRunSummary(t, directory, strings.Repeat("c", 40), tree, 1, "conformance", conformanceCases)
			writeRunSummary(t, directory, commit, tree, 1, "persistence", persistenceCases)
		},
		"stale extra": func(directory string) {
			writeRunSummary(t, directory, commit, tree, 1, "conformance", conformanceCases)
			writeRunSummary(t, directory, commit, tree, 1, "persistence", persistenceCases)
			if err := os.WriteFile(filepath.Join(directory, "stale.json"), []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, arrange := range tests {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			arrange(directory)
			if _, err := readMeasurements(directory, 1, commit, tree); err == nil {
				t.Fatal("mutated run evidence was accepted")
			}
		})
	}
}

func TestDecodeRegistryEvidenceRequiresExactCandidateDigests(t *testing.T) {
	catalog := testCatalog()
	data := marshalJSON(t, validRegistryEvidence(catalog))
	evidence, err := decodeRegistryEvidence(data, catalog)
	if err != nil {
		t.Fatalf("decode valid registry evidence: %v", err)
	}
	if evidence.exactCandidates != 3 || evidence.scopedArtifactsNotDiscovered != 3 {
		t.Fatalf("unexpected registry evidence: %+v", evidence)
	}

	mutated := validRegistryEvidence(catalog)
	mutated["candidates"].([]map[string]any)[0]["linux_arm64_digest"] = "sha256:" + strings.Repeat("f", 64)
	if _, err := decodeRegistryEvidence(marshalJSON(t, mutated), catalog); err == nil {
		t.Fatal("wrong platform digest was accepted")
	}
}

func TestDecodeRegistryEvidenceDoesNotOverclaimArtifactAbsence(t *testing.T) {
	catalog := testCatalog()
	value := validRegistryEvidence(catalog)
	value["candidates"].([]map[string]any)[0]["referrers_count"] = 1
	evidence, err := decodeRegistryEvidence(marshalJSON(t, value), catalog)
	if err != nil {
		t.Fatalf("decode discoverable artifact evidence: %v", err)
	}
	if evidence.scopedArtifactsNotDiscovered != 2 {
		t.Fatalf("not-discovered count = %d, want 2", evidence.scopedArtifactsNotDiscovered)
	}
}

func TestDecodeArchiveEvidenceUsesLocalRoundTripClaim(t *testing.T) {
	imageID := "sha256:" + strings.Repeat("e", 64)
	value := map[string]any{
		"schema_version":                   1,
		"claim":                            "local_archive_roundtrip",
		"passed":                           true,
		"image_id":                         imageID,
		"archive_bytes":                    1024,
		"cli_network_isolation":            true,
		"daemon_cache_could_supply_layers": true,
	}
	evidence, err := decodeArchiveEvidence(marshalJSON(t, value), imageID)
	if err != nil || !evidence.passed {
		t.Fatalf("decode valid archive evidence: %+v %v", evidence, err)
	}
	if strings.Contains(evidence.claim, "airgap") || strings.Contains(evidence.claim, "offline") {
		t.Fatalf("archive claim overstates proof: %q", evidence.claim)
	}

	value["cli_network_isolation"] = false
	if _, err := decodeArchiveEvidence(marshalJSON(t, value), imageID); err == nil {
		t.Fatal("archive evidence without network-isolated import was accepted")
	}
}

func TestDeriveAssertionsUsesObservedCountsAndExpectedRejection(t *testing.T) {
	counts := make(map[string]int, len(conformanceCases)+len(persistenceCases))
	for _, name := range append(append([]string(nil), conformanceCases...), persistenceCases...) {
		counts[name] = 5
	}
	values := measurements{repetitions: 5, caseCounts: counts}
	registry := registryEvidence{exactCandidates: 3, scopedArtifactsNotDiscovered: 3}
	archive := archiveEvidence{passed: true, claim: "local_archive_roundtrip"}
	catalog := testCatalog()
	evaluation, err := objectstore.EvaluateCatalog(catalog)
	if err != nil {
		t.Fatal(err)
	}
	assertions := deriveAssertions(values, registry, archive, evaluation, cleanupEvidence{}, true, 5)
	assertionByName := make(map[string]bool, len(assertions))
	for _, assertion := range assertions {
		assertionByName[assertion.Name] = assertion.Passed
		if assertion.Name == "tdd.red_observed" {
			t.Fatal("unsupported automated TDD receipt was emitted")
		}
	}
	for _, name := range []string{
		"candidate.minio_correctly_rejected",
		"registry.multiarch_exact",
		"s3.checksum_put_head_get",
		"s3.restart_persistence",
		"archive.local_archive_roundtrip",
		"secret.exact_sentinel_scan",
	} {
		if !assertionByName[name] {
			t.Fatalf("assertion %q did not pass", name)
		}
	}
}

func writeRunSummary(t *testing.T, directory, commit, tree string, iteration int, phase string, cases []string) {
	t.Helper()
	latencies := make(map[string]float64, len(cases))
	for index, name := range cases {
		latencies[name] = float64(index + 1)
	}
	value := map[string]any{
		"schema_version":  1,
		"git_commit":      commit,
		"source_tree":     tree,
		"run_id":          "invocation-12345678-" + string(rune('0'+iteration)),
		"iteration":       iteration,
		"phase":           phase,
		"case_latency_ms": latencies,
		"bytes_verified":  4096,
	}
	data := marshalJSON(t, value)
	path := filepath.Join(directory, "run-"+string(rune('0'+iteration))+"-"+phase+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func testCatalog() objectstore.Catalog {
	candidates := make([]objectstore.Candidate, 0, 3)
	for index, id := range []string{"seaweedfs", "garage", "minio-community"} {
		candidate := completeCandidate(id, byte('a'+index), byte('1'+index), byte('4'+index))
		if id == "minio-community" {
			candidate.Maintenance.Active = testBoolPointer(false)
			candidate.Distribution.Status = "source-only"
		}
		candidates = append(candidates, candidate)
	}
	seaweed := candidates[0]
	return objectstore.Catalog{
		SchemaVersion: 1,
		ResearchedAt:  "2026-07-16",
		Selection: objectstore.Selection{
			CandidateID:        "seaweedfs",
			ImmutableReference: "docker.io/example/seaweedfs@" + seaweed.Image.IndexDigest,
			Scope:              "spike only",
			ReleaseRequirement: "mirror and sign before E18",
		},
		Candidates: candidates,
	}
}

func completeCandidate(id string, indexByte, amdByte, armByte byte) objectstore.Candidate {
	return objectstore.Candidate{
		ID: id, Name: id, Version: "v1",
		PrimarySource: objectstore.PrimarySourceEvidence{URL: "https://example.invalid/release", Commit: strings.Repeat("a", 40)},
		License:       objectstore.LicenseEvidence{SPDX: "Apache-2.0", ReceiptURL: "https://example.invalid/license"},
		Maintenance:   objectstore.MaintenanceEvidence{Active: testBoolPointer(true), CheckedAt: "2026-07-16", Status: "active", ReceiptURL: "https://example.invalid/releases"},
		Image: objectstore.ImageEvidence{
			TagReference: "docker.io/example/" + id + ":v1",
			IndexDigest:  "sha256:" + strings.Repeat(string(indexByte), 64),
			Platforms: []objectstore.PlatformDigest{
				{OS: "linux", Architecture: "amd64", Digest: "sha256:" + strings.Repeat(string(amdByte), 64)},
				{OS: "linux", Architecture: "arm64", Digest: "sha256:" + strings.Repeat(string(armByte), 64)},
			},
		},
		Readiness:     objectstore.ReadinessEvidence{Command: "GET /health", Semantics: "200/503"},
		S3Conformance: objectstore.CommandEvidence{Command: "go test"},
		Offline:       objectstore.OfflineEvidence{ExportCommand: "docker save", ImportCommand: "docker load", Claim: "local archive"},
		SupplyChain:   objectstore.SupplyChainEvidence{CheckedAt: "2026-07-16", Status: "not discovered", CheckMethod: "OCI referrers"},
		Distribution:  objectstore.DistributionEvidence{Status: "container"},
	}
}

func validRegistryEvidence(catalog objectstore.Catalog) map[string]any {
	candidates := make([]map[string]any, 0, len(catalog.Candidates))
	for _, candidate := range catalog.Candidates {
		candidates = append(candidates, map[string]any{
			"id":                    candidate.ID,
			"index_digest":          candidate.Image.IndexDigest,
			"linux_amd64_digest":    candidate.Image.Platforms[0].Digest,
			"linux_arm64_digest":    candidate.Image.Platforms[1].Digest,
			"referrers_status":      "queried",
			"referrers_count":       0,
			"legacy_artifact_count": 0,
		})
	}
	return map[string]any{
		"schema_version": 1,
		"checked_at":     "2026-07-16T10:00:00Z",
		"candidates":     candidates,
	}
}

func marshalJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func testBoolPointer(value bool) *bool {
	return &value
}
