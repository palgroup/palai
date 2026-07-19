// Package uat holds the local-live UAT case runner and the evidence verifier. The verifier
// (this file) is Docker-free pure logic so it rides make verify; the case runner
// (local_live_test.go) is behind the `uat` build tag and drives the real stack.
package uat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// Finding is one reason an evidence bundle fails verification. Case is "" for a
// release-level finding.
type Finding struct {
	Case   string
	Kind   string // missing | invalid | secret
	Detail string
}

func (f Finding) String() string {
	if f.Case == "" {
		return fmt.Sprintf("[%s] %s", f.Kind, f.Detail)
	}
	return fmt.Sprintf("%s [%s] %s", f.Case, f.Kind, f.Detail)
}

// Summary is the verified state of one release bundle — the numbers make evidence-verify.
type Summary struct {
	Passed         int
	Failed         int
	Missing        int
	SecretFindings int
	Findings       []Finding
}

// OK reports a clean bundle: every case passed with no missing field and no leaked secret.
func (s Summary) OK() bool { return s.Failed == 0 && s.Missing == 0 && s.SecretFindings == 0 }

// String renders the operator summary line evidence-verify prints.
func (s Summary) String() string {
	return fmt.Sprintf("%d passed, %d failed, %d missing, %d secret findings",
		s.Passed, s.Failed, s.Missing, s.SecretFindings)
}

// evidenceManifest mirrors protocols/schemas/evidence/manifest.json. Missing required
// fields decode to the zero value, which the verifier reports rather than tolerating.
type evidenceManifest struct {
	Release    string         `json:"release"`
	GitSHA     string         `json:"git_sha"`
	APIVersion string         `json:"api_version"`
	Migration  string         `json:"migration"`
	CapturedAt string         `json:"captured_at"`
	Cases      []evidenceCase `json:"cases"`
}

type evidenceCase struct {
	ID                string         `json:"id"`
	Status            string         `json:"status"`
	ProofClass        string         `json:"proof_class"`
	RunID             string         `json:"run_id"`
	ImageDigest       string         `json:"image_digest"`
	ProviderRequestID string         `json:"provider_request_id"`
	MTLSEnroll        string         `json:"mtls_enroll"`
	Terminal          evidenceTerm   `json:"terminal"`
	Usage             map[string]int `json:"usage"`
	DBAssertions      []string       `json:"db_assertions"`
	Checksum          string         `json:"checksum"`
}

type evidenceTerm struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

// secretPattern matches a credential-shaped token (an OpenAI-style sk- key), so a plaintext
// credential fails the redaction scan even when the exact value is not supplied as a needle.
var secretPattern = regexp.MustCompile(`sk-[A-Za-z0-9_-]{12,}`)

// checksumPattern is the required checksum shape (sha256:<64 hex>).
var checksumPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// VerifyManifest checks one evidence manifest against the required-field and redaction
// contract. It returns every finding; an empty slice is a clean pass. secrets are extra
// literal needles (e.g. a run's real credential) that must never appear in the manifest.
func VerifyManifest(raw []byte, secrets []string) []Finding {
	var findings []Finding

	// Redaction is a hard gate regardless of structure: a leaked credential fails the bundle.
	for _, needle := range secrets {
		if needle != "" && bytes.Contains(raw, []byte(needle)) {
			findings = append(findings, Finding{Kind: "secret", Detail: "manifest contains a supplied credential value"})
		}
	}
	if secretPattern.Match(raw) {
		findings = append(findings, Finding{Kind: "secret", Detail: "manifest contains a credential-shaped token (sk-...)"})
	}

	var m evidenceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return append(findings, Finding{Kind: "invalid", Detail: "manifest is not valid JSON: " + err.Error()})
	}

	miss := func(cond bool, field, c string) {
		if cond {
			findings = append(findings, Finding{Case: c, Kind: "missing", Detail: field})
		}
	}
	miss(m.Release == "", "release", "")
	miss(m.GitSHA == "", "git_sha", "")
	miss(m.APIVersion == "", "api_version", "")
	miss(m.Migration == "", "migration", "")
	miss(len(m.Cases) == 0, "cases", "")

	for _, c := range m.Cases {
		miss(c.ID == "", "id", c.ID)
		miss(c.RunID == "", "run_id", c.ID)
		miss(c.ImageDigest == "", "image_digest", c.ID)
		miss(c.ProviderRequestID == "", "provider_request_id", c.ID)
		miss(c.MTLSEnroll == "", "mtls_enroll", c.ID)
		miss(len(c.DBAssertions) == 0, "db_assertions", c.ID)
		miss(c.Checksum == "", "checksum", c.ID)
		if c.Checksum != "" && !checksumPattern.MatchString(c.Checksum) {
			findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: "checksum is not sha256:<64 hex>"})
		}
		if c.Terminal.Count != 1 {
			findings = append(findings, Finding{Case: c.ID, Kind: "invalid",
				Detail: fmt.Sprintf("terminal count = %d, want exactly 1", c.Terminal.Count)})
		}
	}
	return findings
}

// VerifyRelease verifies the manifest.json in a release directory and rolls the findings
// into the operator summary. A missing manifest is a failed bundle, not a crash.
func VerifyRelease(releaseDir string, secrets []string) (Summary, error) {
	raw, err := os.ReadFile(filepath.Join(releaseDir, "manifest.json"))
	if err != nil {
		return Summary{}, fmt.Errorf("read evidence manifest: %w", err)
	}
	findings := VerifyManifest(raw, secrets)

	var m evidenceManifest
	_ = json.Unmarshal(raw, &m)

	// A case is failed if it carries any finding or its recorded status is not PASS.
	failedCases := map[string]bool{}
	summary := Summary{Findings: findings}
	for _, f := range findings {
		switch f.Kind {
		case "missing":
			summary.Missing++
		case "secret":
			summary.SecretFindings++
		}
		if f.Case != "" {
			failedCases[f.Case] = true
		}
	}
	for _, c := range m.Cases {
		if failedCases[c.ID] || c.Status != "PASS" {
			summary.Failed++
			continue
		}
		summary.Passed++
	}
	// A release-level finding (bad git_sha, leaked secret) fails the whole bundle even when
	// every case looks clean, so a zero-case pass is never reported as OK.
	if summary.Passed > 0 && (summary.SecretFindings > 0 || releaseLevelMissing(findings)) {
		summary.Failed += summary.Passed
		summary.Passed = 0
	}
	return summary, nil
}

// releaseLevelMissing reports whether any finding is a release-level (case-less) problem.
func releaseLevelMissing(findings []Finding) bool {
	for _, f := range findings {
		if f.Case == "" && (f.Kind == "missing" || f.Kind == "invalid") {
			return true
		}
	}
	return false
}
