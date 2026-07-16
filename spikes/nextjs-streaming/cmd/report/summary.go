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
	"strings"

	spikereport "github.com/palgroup/palai/spikes/internal/report"
)

var scanCategories = []string{
	"build_output",
	"downstream_response",
	"next_server_log",
	"server_bundle",
	"source_file",
	"source_map",
	"static_chunk",
}

var expectedOutcomeNames = []string{
	"abort.explicit_cancel_not_called",
	"abort.upstream_transport_prompt",
	"harness.output_capture_bounded",
	"reconnect.last_event_id_exact",
	"runtime.next_start",
	"secret.scan_targets_clean",
	"secret.upstream_authorization_only",
	"stream.first_frame_unbuffered",
	"stream.ordered_canonical_frames",
	"toolchain.exact_runtime_versions",
	"toolchain.typescript7_effective_gate",
	"upstream.error_response_redacted",
}

var (
	hexCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	hexDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	nextBuildPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	runIDPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{7,127}$`)
)

type buildContract struct {
	NextVersion                     string `json:"next_version"`
	NextBuildID                     string `json:"next_build_id"`
	NextLegacyTypeScriptAPIBypassed bool   `json:"next_legacy_typescript_api_bypassed"`
	ReactDOMVersion                 string `json:"react_dom_version"`
	ReactVersion                    string `json:"react_version"`
	SchemaVersion                   int    `json:"schema_version"`
	ServerOnlyVersion               string `json:"server_only_version"`
	SourceFingerprint               string `json:"source_fingerprint"`
	TypeScriptNegativeProbe         bool   `json:"typescript_negative_probe_rejected"`
	TypeScriptProjectTypecheck      bool   `json:"typescript_project_typecheck_passed"`
	TypeScriptVersion               string `json:"typescript_version"`
}

type runOutcome struct {
	Detail string `json:"detail"`
	Name   string `json:"name"`
	Passed *bool  `json:"passed"`
}

type processResult struct {
	Completed *bool `json:"completed"`
	ExitCode  *int  `json:"exit_code"`
}

type captureLimits struct {
	CommandOutputBytesPerStream int `json:"command_output_bytes_per_stream"`
	NextServerLogBytesPerStream int `json:"next_server_log_bytes_per_stream"`
}

type runSummary struct {
	AbortToUpstreamCloseMS *float64       `json:"abort_to_upstream_close_ms"`
	BuildContract          buildContract  `json:"build_contract"`
	CaptureLimits          captureLimits  `json:"capture_limits"`
	GitCommit              string         `json:"git_commit"`
	InvocationID           string         `json:"invocation_id"`
	Iteration              int            `json:"iteration"`
	Outcomes               []runOutcome   `json:"outcomes"`
	ProcessResult          processResult  `json:"process_result"`
	ProductionServer       string         `json:"production_server"`
	ScanTargets            map[string]int `json:"scan_targets"`
	SchemaVersion          int            `json:"schema_version"`
	SourceTree             string         `json:"source_tree"`
	TimeToFirstFrameMS     *float64       `json:"time_to_first_frame_ms"`
}

type measurements struct {
	abortMS          []float64
	assertionsPerRun int
	buildContract    buildContract
	firstFrameMS     []float64
	outcomeCounts    map[string]int
	scanTargets      map[string]int
}

func decodeRunSummary(data []byte, commit, tree, runID string, iteration int) (runSummary, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var summary runSummary
	if err := decoder.Decode(&summary); err != nil {
		return runSummary{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return runSummary{}, errors.New("trailing JSON value")
	}
	if err := validateSummary(summary, commit, tree, runID, iteration); err != nil {
		return runSummary{}, err
	}
	return summary, nil
}

func readMeasurements(
	runDirectory string,
	repetitions int,
	commit string,
	sourceTree string,
	runID string,
) (measurements, error) {
	values := measurements{
		abortMS:       make([]float64, 0, repetitions),
		firstFrameMS:  make([]float64, 0, repetitions),
		outcomeCounts: make(map[string]int, len(expectedOutcomeNames)),
	}
	entries, err := os.ReadDir(runDirectory)
	if err != nil {
		return measurements{}, fmt.Errorf("read run summary directory: %w", err)
	}
	if len(entries) != repetitions {
		return measurements{}, fmt.Errorf(
			"run summary directory contains %d entries, want %d",
			len(entries),
			repetitions,
		)
	}
	expectedFiles := make(map[string]struct{}, repetitions)
	for iteration := 1; iteration <= repetitions; iteration++ {
		expectedFiles[fmt.Sprintf("run-%d.json", iteration)] = struct{}{}
	}
	for _, entry := range entries {
		if _, ok := expectedFiles[entry.Name()]; !ok || entry.IsDir() {
			return measurements{}, fmt.Errorf("unexpected run summary entry %q", entry.Name())
		}
	}
	for iteration := 1; iteration <= repetitions; iteration++ {
		path := filepath.Join(runDirectory, fmt.Sprintf("run-%d.json", iteration))
		data, err := os.ReadFile(path)
		if err != nil {
			return measurements{}, fmt.Errorf("read run summary %d: %w", iteration, err)
		}
		summary, err := decodeRunSummary(data, commit, sourceTree, runID, iteration)
		if err != nil {
			return measurements{}, fmt.Errorf("validate run summary %d: %w", iteration, err)
		}
		if iteration == 1 {
			values.assertionsPerRun = len(summary.Outcomes)
			values.buildContract = summary.BuildContract
			values.scanTargets = summary.ScanTargets
		} else if len(summary.Outcomes) != values.assertionsPerRun ||
			summary.BuildContract != values.buildContract ||
			!equalCounts(summary.ScanTargets, values.scanTargets) {
			return measurements{}, fmt.Errorf("run summary %d changed assertion or scan target counts", iteration)
		}
		values.abortMS = append(values.abortMS, *summary.AbortToUpstreamCloseMS)
		values.firstFrameMS = append(values.firstFrameMS, *summary.TimeToFirstFrameMS)
		for _, outcome := range summary.Outcomes {
			if outcome.Passed != nil && *outcome.Passed {
				values.outcomeCounts[outcome.Name]++
			}
		}
	}
	return values, nil
}

func deriveAssertions(counts map[string]int, repetitions int) []spikereport.Assertion {
	assertions := make([]spikereport.Assertion, 0, len(expectedOutcomeNames))
	for _, name := range expectedOutcomeNames {
		observed := counts[name]
		assertions = append(assertions, spikereport.Assertion{
			Name:   name,
			Passed: observed == repetitions,
			Detail: fmt.Sprintf("%d/%d observed", observed, repetitions),
		})
	}
	return assertions
}

func validateSummary(summary runSummary, commit, tree, runID string, iteration int) error {
	if summary.SchemaVersion != 2 || summary.ProductionServer != "next start" {
		return errors.New("unexpected live test contract")
	}
	if !hexCommitPattern.MatchString(summary.GitCommit) || summary.GitCommit != commit {
		return errors.New("run summary git commit does not match current source")
	}
	if !hexCommitPattern.MatchString(summary.SourceTree) || summary.SourceTree != tree {
		return errors.New("run summary source tree does not match current source")
	}
	if !runIDPattern.MatchString(summary.InvocationID) || summary.InvocationID != runID {
		return errors.New("run summary invocation does not match current invocation")
	}
	if summary.Iteration != iteration {
		return errors.New("run summary iteration does not match its expected iteration")
	}
	if summary.ProcessResult.Completed == nil || !*summary.ProcessResult.Completed ||
		summary.ProcessResult.ExitCode == nil || *summary.ProcessResult.ExitCode != 0 {
		return errors.New("run summary process did not complete successfully")
	}
	if summary.AbortToUpstreamCloseMS == nil {
		return errors.New("abort latency is required")
	}
	abortMS := *summary.AbortToUpstreamCloseMS
	if math.IsNaN(abortMS) || math.IsInf(abortMS, 0) || abortMS < 0 || abortMS >= 500 {
		return errors.New("abort latency is outside the prompt-close bound")
	}
	if summary.TimeToFirstFrameMS == nil {
		return errors.New("time to first frame is required")
	}
	firstFrameMS := *summary.TimeToFirstFrameMS
	if math.IsNaN(firstFrameMS) || math.IsInf(firstFrameMS, 0) || firstFrameMS < 0 {
		return errors.New("time to first frame is invalid")
	}
	if err := validateOutcomes(summary.Outcomes); err != nil {
		return err
	}
	contract := summary.BuildContract
	if contract.SchemaVersion != 2 || contract.NextVersion != "16.2.10" ||
		!nextBuildPattern.MatchString(contract.NextBuildID) ||
		contract.ReactVersion != "19.2.7" || contract.ReactDOMVersion != "19.2.7" ||
		contract.ServerOnlyVersion != "0.0.1" || contract.TypeScriptVersion != "7.0.2" ||
		!hexDigestPattern.MatchString(contract.SourceFingerprint) ||
		!contract.NextLegacyTypeScriptAPIBypassed || !contract.TypeScriptNegativeProbe || !contract.TypeScriptProjectTypecheck {
		return errors.New("TypeScript 7 effective gate was not proven")
	}
	if summary.CaptureLimits.CommandOutputBytesPerStream != 1024*1024 ||
		summary.CaptureLimits.NextServerLogBytesPerStream != 256*1024 {
		return errors.New("child output capture limits changed")
	}
	if len(summary.ScanTargets) != len(scanCategories) {
		return errors.New("secret scan categories changed")
	}
	for _, category := range scanCategories {
		if summary.ScanTargets[category] < 1 {
			return fmt.Errorf("secret scan category %s has no targets", category)
		}
	}
	return nil
}

func validateOutcomes(outcomes []runOutcome) error {
	if len(outcomes) != len(expectedOutcomeNames) {
		return errors.New("observed outcome set is incomplete")
	}
	expected := make(map[string]struct{}, len(expectedOutcomeNames))
	for _, name := range expectedOutcomeNames {
		expected[name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(outcomes))
	for _, outcome := range outcomes {
		if _, ok := expected[outcome.Name]; !ok {
			return fmt.Errorf("unexpected observed outcome %q", outcome.Name)
		}
		if _, duplicate := seen[outcome.Name]; duplicate {
			return fmt.Errorf("duplicate observed outcome %q", outcome.Name)
		}
		if outcome.Passed == nil || !*outcome.Passed {
			return fmt.Errorf("observed outcome %q did not pass", outcome.Name)
		}
		if strings.TrimSpace(outcome.Detail) == "" {
			return fmt.Errorf("observed outcome %q has no detail", outcome.Name)
		}
		seen[outcome.Name] = struct{}{}
	}
	return nil
}

func equalCounts(left, right map[string]int) bool {
	if len(left) != len(right) {
		return false
	}
	for name, value := range left {
		if right[name] != value {
			return false
		}
	}
	return true
}

func min(values []float64) float64 {
	minimum := values[0]
	for _, value := range values[1:] {
		if value < minimum {
			minimum = value
		}
	}
	return minimum
}

func max(values []float64) float64 {
	maximum := values[0]
	for _, value := range values[1:] {
		if value > maximum {
			maximum = value
		}
	}
	return maximum
}

func mean(values []float64) float64 {
	total := 0.0
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}
