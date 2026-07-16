package objectstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var (
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type Catalog struct {
	SchemaVersion int         `json:"schema_version"`
	ResearchedAt  string      `json:"researched_at"`
	Selection     Selection   `json:"selection"`
	Candidates    []Candidate `json:"candidates"`
}

type Selection struct {
	CandidateID        string `json:"candidate_id"`
	ImmutableReference string `json:"immutable_reference"`
	Scope              string `json:"scope"`
	ReleaseRequirement string `json:"release_requirement"`
}

type Candidate struct {
	ID            string                `json:"id"`
	Name          string                `json:"name"`
	Version       string                `json:"version"`
	PrimarySource PrimarySourceEvidence `json:"primary_source"`
	License       LicenseEvidence       `json:"license"`
	Maintenance   MaintenanceEvidence   `json:"maintenance"`
	Image         ImageEvidence         `json:"image"`
	Readiness     ReadinessEvidence     `json:"readiness"`
	S3Conformance CommandEvidence       `json:"s3_conformance"`
	Offline       OfflineEvidence       `json:"offline"`
	SupplyChain   SupplyChainEvidence   `json:"supply_chain"`
	Distribution  DistributionEvidence  `json:"distribution"`
}

type PrimarySourceEvidence struct {
	URL    string `json:"url"`
	Commit string `json:"commit"`
}

type LicenseEvidence struct {
	SPDX       string `json:"spdx"`
	ReceiptURL string `json:"receipt_url"`
}

type MaintenanceEvidence struct {
	Active     *bool  `json:"active"`
	CheckedAt  string `json:"checked_at"`
	Status     string `json:"status"`
	ReceiptURL string `json:"receipt_url"`
}

type ImageEvidence struct {
	TagReference string           `json:"tag_reference"`
	IndexDigest  string           `json:"index_digest"`
	Platforms    []PlatformDigest `json:"platforms"`
}

type PlatformDigest struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Digest       string `json:"digest"`
}

type ReadinessEvidence struct {
	Command   string `json:"command"`
	Semantics string `json:"semantics"`
}

type CommandEvidence struct {
	Command string `json:"command"`
}

type OfflineEvidence struct {
	ExportCommand string `json:"export_command"`
	ImportCommand string `json:"import_command"`
	Claim         string `json:"claim"`
}

type SupplyChainEvidence struct {
	CheckedAt   string `json:"checked_at"`
	Status      string `json:"status"`
	CheckMethod string `json:"check_method"`
}

type DistributionEvidence struct {
	Status        string `json:"status"`
	PolicyConcern string `json:"policy_concern"`
}

type CandidateResult struct {
	ID       string
	Complete bool
	Eligible bool
	Issues   []string
}

type Evaluation struct {
	Selected   CandidateResult
	Candidates []CandidateResult
}

func DecodeCatalog(reader io.Reader) (Catalog, error) {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	var catalog Catalog
	if err := decoder.Decode(&catalog); err != nil {
		return Catalog{}, fmt.Errorf("decode candidate catalog: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Catalog{}, errors.New("decode candidate catalog: trailing JSON value")
	}
	return catalog, nil
}

func EvaluateCandidate(candidate Candidate) CandidateResult {
	issues := make([]string, 0)
	requireText := func(path, value string) {
		if strings.TrimSpace(value) == "" {
			issues = append(issues, path)
		}
	}
	requireURL := func(path, value string) {
		parsed, err := url.Parse(value)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
			issues = append(issues, path)
		}
	}
	requireDate := func(path, value string) {
		if _, err := time.Parse(time.DateOnly, value); err != nil {
			issues = append(issues, path)
		}
	}

	requireText("id", candidate.ID)
	requireText("name", candidate.Name)
	requireText("version", candidate.Version)
	requireURL("primary_source.url", candidate.PrimarySource.URL)
	if !commitPattern.MatchString(candidate.PrimarySource.Commit) {
		issues = append(issues, "primary_source.commit")
	}
	requireText("license.spdx", candidate.License.SPDX)
	requireURL("license.receipt_url", candidate.License.ReceiptURL)
	requireDate("maintenance.checked_at", candidate.Maintenance.CheckedAt)
	if candidate.Maintenance.Active == nil {
		issues = append(issues, "maintenance.active")
	}
	requireText("maintenance.status", candidate.Maintenance.Status)
	requireURL("maintenance.receipt_url", candidate.Maintenance.ReceiptURL)
	requireText("image.tag_reference", candidate.Image.TagReference)
	if !digestPattern.MatchString(candidate.Image.IndexDigest) {
		issues = append(issues, "image.index_digest")
	}
	platforms := make(map[string]bool, len(candidate.Image.Platforms))
	for _, platform := range candidate.Image.Platforms {
		if digestPattern.MatchString(platform.Digest) {
			platforms[platform.OS+"/"+platform.Architecture] = true
		}
	}
	for _, platform := range []string{"linux/amd64", "linux/arm64"} {
		if !platforms[platform] {
			issues = append(issues, "image."+strings.ReplaceAll(platform, "/", "_"))
		}
	}
	requireText("readiness.command", candidate.Readiness.Command)
	requireText("readiness.semantics", candidate.Readiness.Semantics)
	requireText("s3_conformance.command", candidate.S3Conformance.Command)
	requireText("offline.export_command", candidate.Offline.ExportCommand)
	requireText("offline.import_command", candidate.Offline.ImportCommand)
	requireText("offline.claim", candidate.Offline.Claim)
	requireDate("supply_chain.checked_at", candidate.SupplyChain.CheckedAt)
	requireText("supply_chain.status", candidate.SupplyChain.Status)
	requireText("supply_chain.check_method", candidate.SupplyChain.CheckMethod)
	requireText("distribution.status", candidate.Distribution.Status)

	complete := len(issues) == 0
	eligible := complete && candidate.Maintenance.Active != nil && *candidate.Maintenance.Active && candidate.Distribution.Status != "source-only"
	return CandidateResult{ID: candidate.ID, Complete: complete, Eligible: eligible, Issues: issues}
}

func EvaluateCatalog(catalog Catalog) (Evaluation, error) {
	if catalog.SchemaVersion != 1 {
		return Evaluation{}, errors.New("candidate catalog schema_version must be 1")
	}
	if _, err := time.Parse(time.DateOnly, catalog.ResearchedAt); err != nil {
		return Evaluation{}, errors.New("candidate catalog researched_at must be an ISO date")
	}
	if len(catalog.Candidates) < 1 {
		return Evaluation{}, errors.New("candidate catalog is empty")
	}
	results := make([]CandidateResult, 0, len(catalog.Candidates))
	candidates := make(map[string]Candidate, len(catalog.Candidates))
	for _, candidate := range catalog.Candidates {
		if _, duplicate := candidates[candidate.ID]; duplicate {
			return Evaluation{}, fmt.Errorf("duplicate candidate %q", candidate.ID)
		}
		result := EvaluateCandidate(candidate)
		if !result.Complete {
			return Evaluation{}, fmt.Errorf("candidate %q missing evidence: %s", candidate.ID, strings.Join(result.Issues, ", "))
		}
		candidates[candidate.ID] = candidate
		results = append(results, result)
	}
	selected, ok := candidates[catalog.Selection.CandidateID]
	if !ok {
		return Evaluation{}, errors.New("selected candidate is not in the catalog")
	}
	selectedResult := EvaluateCandidate(selected)
	if !selectedResult.Eligible {
		return Evaluation{}, errors.New("selected candidate is not eligible")
	}
	expectedReference := repositoryWithoutTag(selected.Image.TagReference) + "@" + selected.Image.IndexDigest
	if catalog.Selection.ImmutableReference != expectedReference || !strings.Contains(catalog.Selection.ImmutableReference, "@sha256:") {
		return Evaluation{}, errors.New("selection must use the selected immutable OCI index")
	}
	if strings.TrimSpace(catalog.Selection.Scope) == "" || strings.TrimSpace(catalog.Selection.ReleaseRequirement) == "" {
		return Evaluation{}, errors.New("selection scope and release requirement are required")
	}
	return Evaluation{Selected: selectedResult, Candidates: results}, nil
}

func repositoryWithoutTag(reference string) string {
	slash := strings.LastIndex(reference, "/")
	colon := strings.LastIndex(reference, ":")
	if colon > slash {
		return reference[:colon]
	}
	return reference
}
