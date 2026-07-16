package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	hexIDPattern       = regexp.MustCompile(`^[0-9a-f]{40}$`)
	spikePattern       = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	namePattern        = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	digestPattern      = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	secretKeyPattern   = regexp.MustCompile(`(?i)(OPENAI_API_KEY|ANTHROPIC_API_KEY|ANTROPHIC_API_KEY|Authorization:[[:space:]]*Bearer|BEGIN[[:space:]]+PRIVATE[[:space:]]+KEY)`)
	secretTokenPattern = regexp.MustCompile(`(^|[^A-Za-z0-9])sk-[A-Za-z0-9]`)
)

type Assertion struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

type Environment struct {
	OS           string            `json:"os"`
	Arch         string            `json:"arch"`
	ToolVersions map[string]string `json:"tool_versions"`
	ImageDigests []string          `json:"image_digests"`
}

type Report struct {
	SchemaVersion int                `json:"schema_version"`
	Spike         string             `json:"spike"`
	GitCommit     string             `json:"git_commit"`
	SourceTree    string             `json:"source_tree"`
	StartedAt     time.Time          `json:"started_at"`
	EndedAt       time.Time          `json:"ended_at"`
	Environment   Environment        `json:"environment"`
	Metrics       map[string]float64 `json:"metrics"`
	Assertions    []Assertion        `json:"assertions"`
	Passed        bool               `json:"passed"`
}

func (r *Report) Finalize() error {
	r.Passed = r.derivedPass()
	return r.validate()
}

func Decode(data []byte) (Report, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var report Report
	if err := decoder.Decode(&report); err != nil {
		return Report{}, fmt.Errorf("decode report: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Report{}, errors.New("decode report: trailing JSON value")
	}
	return report, nil
}

func (r Report) ValidateStored() error {
	if err := r.validate(); err != nil {
		return err
	}
	if r.Passed != r.derivedPass() {
		return errors.New("stored passed value does not match assertions")
	}
	return nil
}

func (r Report) MarshalStable() ([]byte, error) {
	copyReport := r
	copyReport.Environment.ImageDigests = append(make([]string, 0, len(r.Environment.ImageDigests)), r.Environment.ImageDigests...)
	copyReport.Assertions = append([]Assertion(nil), r.Assertions...)
	sort.Strings(copyReport.Environment.ImageDigests)
	sort.Slice(copyReport.Assertions, func(i, j int) bool {
		return copyReport.Assertions[i].Name < copyReport.Assertions[j].Name
	})
	if err := copyReport.Finalize(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(copyReport, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal report: %w", err)
	}
	return append(data, '\n'), nil
}

func (r Report) validate() error {
	if r.SchemaVersion != 1 {
		return fmt.Errorf("schema_version must be 1")
	}
	if !spikePattern.MatchString(r.Spike) {
		return fmt.Errorf("spike must be lower kebab-case")
	}
	if !hexIDPattern.MatchString(r.GitCommit) {
		return fmt.Errorf("git_commit must be a 40-character lowercase hex ID")
	}
	if !hexIDPattern.MatchString(r.SourceTree) {
		return fmt.Errorf("source_tree must be a 40-character lowercase hex ID")
	}
	if r.StartedAt.IsZero() || r.EndedAt.IsZero() || r.EndedAt.Before(r.StartedAt) {
		return fmt.Errorf("timestamps must be non-zero and monotonic")
	}
	if r.Environment.OS == "" || r.Environment.Arch == "" {
		return fmt.Errorf("environment os and arch are required")
	}
	if len(r.Environment.ToolVersions) == 0 {
		return fmt.Errorf("at least one tool version is required")
	}
	for name, version := range r.Environment.ToolVersions {
		if !namePattern.MatchString(name) || strings.TrimSpace(version) == "" {
			return fmt.Errorf("invalid tool version %q", name)
		}
	}
	for _, digest := range r.Environment.ImageDigests {
		if !digestPattern.MatchString(digest) {
			return fmt.Errorf("invalid image digest %q", digest)
		}
	}
	for name, value := range r.Metrics {
		if !namePattern.MatchString(name) {
			return fmt.Errorf("invalid metric name %q", name)
		}
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return fmt.Errorf("metric %q must be finite non-negative", name)
		}
	}
	if len(r.Assertions) == 0 {
		return errors.New("at least one assertion is required")
	}
	seen := make(map[string]struct{}, len(r.Assertions))
	for _, assertion := range r.Assertions {
		if !namePattern.MatchString(assertion.Name) {
			return fmt.Errorf("invalid assertion name %q", assertion.Name)
		}
		if _, exists := seen[assertion.Name]; exists {
			return fmt.Errorf("duplicate assertion name %q", assertion.Name)
		}
		seen[assertion.Name] = struct{}{}
	}
	for _, value := range r.stringValues() {
		if containsSensitiveValue(value) {
			return errors.New("report contains a sensitive or host-identifying value")
		}
	}
	return nil
}

func (r Report) derivedPass() bool {
	passed := len(r.Assertions) > 0
	for _, assertion := range r.Assertions {
		if !assertion.Passed {
			passed = false
		}
	}
	return passed
}

func (r Report) stringValues() []string {
	values := []string{r.Spike, r.Environment.OS, r.Environment.Arch}
	for name, version := range r.Environment.ToolVersions {
		values = append(values, name, version)
	}
	values = append(values, r.Environment.ImageDigests...)
	for name := range r.Metrics {
		values = append(values, name)
	}
	for _, assertion := range r.Assertions {
		values = append(values, assertion.Name, assertion.Detail)
	}
	return values
}

func containsSensitiveValue(value string) bool {
	return secretKeyPattern.MatchString(value) ||
		secretTokenPattern.MatchString(value) ||
		strings.Contains(value, "/Users/") ||
		strings.Contains(value, "/home/")
}
