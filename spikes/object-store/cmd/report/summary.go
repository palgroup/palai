package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	spikereport "github.com/palgroup/palai/spikes/internal/report"
	objectstore "github.com/palgroup/palai/spikes/object-store"
)

var summaryRunIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{8,80}$`)

var expectedConformanceCases = []string{
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

var expectedPersistenceCases = []string{
	"persistence.retained_bytes_checksum",
	"persistence.cleanup",
}

type storedRunSummary struct {
	SchemaVersion int                `json:"schema_version"`
	GitCommit     string             `json:"git_commit"`
	SourceTree    string             `json:"source_tree"`
	RunID         string             `json:"run_id"`
	Iteration     int                `json:"iteration"`
	Phase         string             `json:"phase"`
	CaseLatencyMS map[string]float64 `json:"case_latency_ms"`
	BytesVerified int64              `json:"bytes_verified"`
}

type measurements struct {
	repetitions     int
	caseCounts      map[string]int
	caseLatenciesMS map[string][]float64
	bytesVerified   int64
}

type registryCandidate struct {
	ID                  string `json:"id"`
	IndexDigest         string `json:"index_digest"`
	LinuxAMD64Digest    string `json:"linux_amd64_digest"`
	LinuxARM64Digest    string `json:"linux_arm64_digest"`
	ReferrersStatus     string `json:"referrers_status"`
	ReferrersCount      int    `json:"referrers_count"`
	LegacyArtifactCount int    `json:"legacy_artifact_count"`
}

type registryDocument struct {
	SchemaVersion int                 `json:"schema_version"`
	CheckedAt     string              `json:"checked_at"`
	Candidates    []registryCandidate `json:"candidates"`
}

type registryEvidence struct {
	exactCandidates              int
	scopedArtifactsNotDiscovered int
}

type archiveDocument struct {
	SchemaVersion                int    `json:"schema_version"`
	Claim                        string `json:"claim"`
	Passed                       bool   `json:"passed"`
	ImageID                      string `json:"image_id"`
	ArchiveBytes                 int64  `json:"archive_bytes"`
	CLINetworkIsolation          bool   `json:"cli_network_isolation"`
	DaemonCacheCouldSupplyLayers bool   `json:"daemon_cache_could_supply_layers"`
}

type archiveEvidence struct {
	passed       bool
	claim        string
	archiveBytes int64
}

type cleanupEvidence struct {
	containersBefore int
	containersAfter  int
	volumesBefore    int
	volumesAfter     int
}

func readMeasurements(directory string, repetitions int, commit, tree string) (measurements, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return measurements{}, fmt.Errorf("read run evidence directory: %w", err)
	}
	if repetitions < 1 || len(entries) != repetitions*2 {
		return measurements{}, fmt.Errorf("run evidence file count = %d, want %d", len(entries), repetitions*2)
	}
	expectedFiles := make(map[string]struct{}, repetitions*2)
	for iteration := 1; iteration <= repetitions; iteration++ {
		for _, phase := range []string{"conformance", "persistence"} {
			expectedFiles[fmt.Sprintf("run-%d-%s.json", iteration, phase)] = struct{}{}
		}
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return measurements{}, fmt.Errorf("unexpected run evidence directory %q", entry.Name())
		}
		if _, ok := expectedFiles[entry.Name()]; !ok {
			return measurements{}, fmt.Errorf("unexpected run evidence file %q", entry.Name())
		}
	}

	values := measurements{
		repetitions:     repetitions,
		caseCounts:      make(map[string]int, len(expectedConformanceCases)+len(expectedPersistenceCases)),
		caseLatenciesMS: make(map[string][]float64, len(expectedConformanceCases)+len(expectedPersistenceCases)),
	}
	invocation := ""
	for iteration := 1; iteration <= repetitions; iteration++ {
		for _, phase := range []string{"conformance", "persistence"} {
			path := filepath.Join(directory, fmt.Sprintf("run-%d-%s.json", iteration, phase))
			data, err := os.ReadFile(path)
			if err != nil {
				return measurements{}, fmt.Errorf("read run evidence: %w", err)
			}
			summary, err := decodeRunSummary(data)
			if err != nil {
				return measurements{}, fmt.Errorf("decode run %d %s: %w", iteration, phase, err)
			}
			expectedCases := expectedConformanceCases
			if phase == "persistence" {
				expectedCases = expectedPersistenceCases
			}
			prefix, err := validateRunSummary(summary, commit, tree, iteration, phase, expectedCases)
			if err != nil {
				return measurements{}, fmt.Errorf("validate run %d %s: %w", iteration, phase, err)
			}
			if invocation == "" {
				invocation = prefix
			} else if invocation != prefix {
				return measurements{}, errors.New("run evidence invocation changed")
			}
			values.bytesVerified += summary.BytesVerified
			for name, latency := range summary.CaseLatencyMS {
				values.caseCounts[name]++
				values.caseLatenciesMS[name] = append(values.caseLatenciesMS[name], latency)
			}
		}
	}
	return values, nil
}

func decodeRunSummary(data []byte) (storedRunSummary, error) {
	var value storedRunSummary
	if err := decodeStrict(data, &value); err != nil {
		return storedRunSummary{}, err
	}
	return value, nil
}

func validateRunSummary(summary storedRunSummary, commit, tree string, iteration int, phase string, expectedCases []string) (string, error) {
	if summary.SchemaVersion != 1 || summary.GitCommit != commit || summary.SourceTree != tree ||
		summary.Iteration != iteration || summary.Phase != phase || summary.BytesVerified <= 0 {
		return "", errors.New("run provenance or phase contract changed")
	}
	if !summaryRunIDPattern.MatchString(summary.RunID) {
		return "", errors.New("invalid run ID")
	}
	suffix := fmt.Sprintf("-%d", iteration)
	if !strings.HasSuffix(summary.RunID, suffix) {
		return "", errors.New("run ID is not bound to its iteration")
	}
	prefix := strings.TrimSuffix(summary.RunID, suffix)
	if len(summary.CaseLatencyMS) != len(expectedCases) {
		return "", errors.New("run case set is incomplete")
	}
	expected := make(map[string]struct{}, len(expectedCases))
	for _, name := range expectedCases {
		expected[name] = struct{}{}
	}
	for name, latency := range summary.CaseLatencyMS {
		if _, ok := expected[name]; !ok || math.IsNaN(latency) || math.IsInf(latency, 0) || latency < 0 {
			return "", fmt.Errorf("invalid observed case %q", name)
		}
	}
	return prefix, nil
}

func decodeRegistryEvidence(data []byte, catalog objectstore.Catalog) (registryEvidence, error) {
	var document registryDocument
	if err := decodeStrict(data, &document); err != nil {
		return registryEvidence{}, err
	}
	if document.SchemaVersion != 1 || len(document.Candidates) != len(catalog.Candidates) {
		return registryEvidence{}, errors.New("registry evidence candidate set changed")
	}
	if _, err := time.Parse(time.RFC3339, document.CheckedAt); err != nil {
		return registryEvidence{}, errors.New("registry evidence timestamp is invalid")
	}
	expected := make(map[string]objectstore.Candidate, len(catalog.Candidates))
	for _, candidate := range catalog.Candidates {
		expected[candidate.ID] = candidate
	}
	seen := make(map[string]struct{}, len(document.Candidates))
	var evidence registryEvidence
	for _, candidate := range document.Candidates {
		catalogCandidate, ok := expected[candidate.ID]
		if !ok {
			return registryEvidence{}, fmt.Errorf("unexpected registry candidate %q", candidate.ID)
		}
		if _, duplicate := seen[candidate.ID]; duplicate {
			return registryEvidence{}, fmt.Errorf("duplicate registry candidate %q", candidate.ID)
		}
		seen[candidate.ID] = struct{}{}
		amd64, arm64 := catalogPlatformDigests(catalogCandidate)
		if candidate.IndexDigest != catalogCandidate.Image.IndexDigest || candidate.LinuxAMD64Digest != amd64 ||
			candidate.LinuxARM64Digest != arm64 || strings.TrimSpace(candidate.ReferrersStatus) == "" ||
			candidate.ReferrersCount < 0 || candidate.LegacyArtifactCount < 0 {
			return registryEvidence{}, fmt.Errorf("registry evidence differs for %q", candidate.ID)
		}
		evidence.exactCandidates++
		if candidate.ReferrersCount == 0 && candidate.LegacyArtifactCount == 0 {
			evidence.scopedArtifactsNotDiscovered++
		}
	}
	return evidence, nil
}

func decodeArchiveEvidence(data []byte, imageID string) (archiveEvidence, error) {
	var document archiveDocument
	if err := decodeStrict(data, &document); err != nil {
		return archiveEvidence{}, err
	}
	if document.SchemaVersion != 1 || document.Claim != "local_archive_roundtrip" || !document.Passed ||
		document.ImageID != imageID || document.ArchiveBytes <= 0 || !document.CLINetworkIsolation ||
		!document.DaemonCacheCouldSupplyLayers {
		return archiveEvidence{}, errors.New("archive evidence overstates or does not prove the local round-trip")
	}
	return archiveEvidence{passed: true, claim: document.Claim, archiveBytes: document.ArchiveBytes}, nil
}

func deriveAssertions(
	values measurements,
	registry registryEvidence,
	archive archiveEvidence,
	evaluation objectstore.Evaluation,
	cleanup cleanupEvidence,
	secretScan bool,
	repetitions int,
) []spikereport.Assertion {
	results := make(map[string]objectstore.CandidateResult, len(evaluation.Candidates))
	allComplete := len(evaluation.Candidates) == 3
	for _, candidate := range evaluation.Candidates {
		results[candidate.ID] = candidate
		allComplete = allComplete && candidate.Complete
	}
	minio := results["minio-community"]
	garage := results["garage"]
	cleanupPassed := cleanup.containersBefore == 0 && cleanup.containersAfter == 0 &&
		cleanup.volumesBefore == 0 && cleanup.volumesAfter == 0
	ratio := fmt.Sprintf("%d/%d observed", repetitions, repetitions)
	groupPassed := func(names ...string) bool {
		for _, name := range names {
			if values.caseCounts[name] != repetitions {
				return false
			}
		}
		return true
	}
	return []spikereport.Assertion{
		{Name: "archive.local_archive_roundtrip", Passed: archive.passed && archive.claim == "local_archive_roundtrip", Detail: "network-isolated import returned the same image ID; daemon cache may have supplied layers"},
		{Name: "candidate.catalog_complete", Passed: allComplete, Detail: fmt.Sprintf("%d/3 structurally complete", len(results))},
		{Name: "candidate.garage_policy_review_recorded", Passed: garage.Complete && garage.Eligible, Detail: "distribution-policy review concern recorded without a legal conclusion"},
		{Name: "candidate.minio_correctly_rejected", Passed: minio.Complete && !minio.Eligible, Detail: "complete negative record; archived/source-only default rejection"},
		{Name: "candidate.seaweedfs_selected_immutable", Passed: evaluation.Selected.ID == "seaweedfs" && evaluation.Selected.Eligible, Detail: "exact OCI index selected for spike recommendation"},
		{Name: "docker.cleanup_exact", Passed: cleanupPassed, Detail: fmt.Sprintf("containers=%d/%d volumes=%d/%d", cleanup.containersAfter, cleanup.containersBefore, cleanup.volumesAfter, cleanup.volumesBefore)},
		{Name: "registry.multiarch_exact", Passed: registry.exactCandidates == 3, Detail: fmt.Sprintf("%d/3 exact index+amd64+arm64", registry.exactCandidates)},
		{Name: "s3.auth_credentials_enforced", Passed: groupPassed("auth.wrong_secret_rejected"), Detail: ratio},
		{Name: "s3.bucket_create_head", Passed: groupPassed("bucket.create_and_head"), Detail: ratio},
		{Name: "s3.checksum_put_head_get", Passed: groupPassed("checksum.put_head_get"), Detail: ratio},
		{Name: "s3.conditional_create", Passed: groupPassed("conditional.if_none_match"), Detail: ratio},
		{Name: "s3.delete_not_found", Passed: groupPassed("object.delete_not_found"), Detail: ratio},
		{Name: "s3.multipart_complete_abort", Passed: groupPassed("multipart.complete", "multipart.abort"), Detail: ratio},
		{Name: "s3.range_exact", Passed: groupPassed("range.exact_bytes"), Detail: ratio},
		{Name: "s3.restart_persistence", Passed: groupPassed("persistence.seeded", "persistence.retained_bytes_checksum", "persistence.cleanup"), Detail: ratio},
		{Name: "secret.exact_sentinel_scan", Passed: secretScan, Detail: "generated summaries and final report scanned with exact per-run sentinels"},
		{Name: "supply_chain.scoped_not_discoverable", Passed: registry.scopedArtifactsNotDiscovered == 3, Detail: fmt.Sprintf("%d/3 exact indexes; scoped methods only, not absolute absence", registry.scopedArtifactsNotDiscovered)},
	}
}

func catalogPlatformDigests(candidate objectstore.Candidate) (amd64 string, arm64 string) {
	for _, platform := range candidate.Image.Platforms {
		if platform.OS != "linux" {
			continue
		}
		switch platform.Architecture {
		case "amd64":
			amd64 = platform.Digest
		case "arm64":
			arm64 = platform.Digest
		}
	}
	return amd64, arm64
}

func decodeStrict(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func latencyMetrics(values measurements) map[string]float64 {
	metrics := make(map[string]float64, len(values.caseLatenciesMS)*3)
	names := make([]string, 0, len(values.caseLatenciesMS))
	for name := range values.caseLatenciesMS {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		latencies := values.caseLatenciesMS[name]
		minimum, maximum, total := latencies[0], latencies[0], 0.0
		for _, latency := range latencies {
			minimum = min(minimum, latency)
			maximum = max(maximum, latency)
			total += latency
		}
		metrics["latency."+name+".min_ms"] = minimum
		metrics["latency."+name+".max_ms"] = maximum
		metrics["latency."+name+".mean_ms"] = total / float64(len(latencies))
	}
	return metrics
}
