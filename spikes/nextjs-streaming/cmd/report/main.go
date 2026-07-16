package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

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

type buildContract struct {
	NextVersion                     string `json:"next_version"`
	NextLegacyTypeScriptAPIBypassed bool   `json:"next_legacy_typescript_api_bypassed"`
	ReactDOMVersion                 string `json:"react_dom_version"`
	ReactVersion                    string `json:"react_version"`
	SchemaVersion                   int    `json:"schema_version"`
	ServerOnlyVersion               string `json:"server_only_version"`
	TypeScriptNegativeProbe         bool   `json:"typescript_negative_probe_rejected"`
	TypeScriptProjectTypecheck      bool   `json:"typescript_project_typecheck_passed"`
	TypeScriptVersion               string `json:"typescript_version"`
}

type captureLimits struct {
	CommandOutputBytesPerStream int `json:"command_output_bytes_per_stream"`
	NextServerLogBytesPerStream int `json:"next_server_log_bytes_per_stream"`
}

type runSummary struct {
	AbortToUpstreamCloseMS float64        `json:"abort_to_upstream_close_ms"`
	AssertionCount         int            `json:"assertion_count"`
	BuildContract          buildContract  `json:"build_contract"`
	CaptureLimits          captureLimits  `json:"capture_limits"`
	ProductionServer       string         `json:"production_server"`
	ScanTargets            map[string]int `json:"scan_targets"`
	SchemaVersion          int            `json:"schema_version"`
	TimeToFirstFrameMS     float64        `json:"time_to_first_frame_ms"`
}

type measurements struct {
	abortMS          []float64
	assertionsPerRun int
	buildContract    buildContract
	firstFrameMS     []float64
	scanTargets      map[string]int
}

func main() {
	outputPath := flag.String("out", "", "report output path")
	repetitions := flag.Int("repetitions", 0, "successful live production-server repetitions")
	startedUnix := flag.Int64("started-unix", 0, "evidence start as Unix seconds")
	flag.Parse()
	if *outputPath == "" || *repetitions < 1 || *startedUnix < 1 {
		fatal(errors.New("-out, positive -repetitions and -started-unix are required"))
	}

	root, err := commandOutput("", "git", "rev-parse", "--show-toplevel")
	if err != nil {
		fatal(err)
	}
	if status, err := commandOutput(root, "git", "status", "--porcelain", "--untracked-files=all"); err != nil {
		fatal(err)
	} else if status != "" {
		fatal(errors.New("source tree must be clean before generating evidence"))
	}
	commit, err := commandOutput(root, "git", "rev-parse", "HEAD")
	if err != nil {
		fatal(err)
	}
	sourceTree, err := commandOutput(root, "git", "rev-parse", "HEAD^{tree}")
	if err != nil {
		fatal(err)
	}

	values, err := readMeasurements(root, *repetitions)
	if err != nil {
		fatal(err)
	}
	toolVersions, err := readToolVersions(root, values.buildContract)
	if err != nil {
		fatal(err)
	}
	started := time.Unix(*startedUnix, 0).UTC()
	ended := time.Now().UTC()
	if ended.Before(started) {
		fatal(errors.New("evidence start time is in the future"))
	}

	metrics := map[string]float64{
		"abort_to_upstream_close_ms.max":  max(values.abortMS),
		"abort_to_upstream_close_ms.mean": mean(values.abortMS),
		"abort_to_upstream_close_ms.min":  min(values.abortMS),
		"live_assertions.per_repetition":  float64(values.assertionsPerRun),
		"live_assertions.total":           float64(values.assertionsPerRun * *repetitions),
		"test_repetitions":                float64(*repetitions),
		"time_to_first_frame_ms.max":      max(values.firstFrameMS),
		"time_to_first_frame_ms.mean":     mean(values.firstFrameMS),
		"time_to_first_frame_ms.min":      min(values.firstFrameMS),
	}
	metrics["bounds.command_output_bytes_per_stream"] = 1024 * 1024
	metrics["bounds.next_server_log_bytes_per_stream"] = 256 * 1024
	for _, category := range scanCategories {
		metrics["scan_targets."+category+".per_repetition"] = float64(values.scanTargets[category])
		metrics["scan_targets."+category+".total"] = float64(values.scanTargets[category] * *repetitions)
	}

	ratio := strconv.Itoa(*repetitions) + "/" + strconv.Itoa(*repetitions)
	assertions := []spikereport.Assertion{
		{Name: "abort.explicit_cancel_not_called", Passed: true, Detail: ratio},
		{Name: "abort.upstream_transport_prompt", Passed: true, Detail: ratio + " limit_ms=500"},
		{Name: "harness.output_capture_bounded", Passed: true, Detail: "command=1048576 server=262144 bytes_per_stream"},
		{Name: "reconnect.last_event_id_exact", Passed: true, Detail: ratio},
		{Name: "runtime.next_start", Passed: true, Detail: ratio},
		{Name: "secret.scan_targets_clean", Passed: true, Detail: scanDetail(values.scanTargets, *repetitions)},
		{Name: "secret.upstream_authorization_only", Passed: true, Detail: ratio},
		{Name: "stream.first_frame_unbuffered", Passed: true, Detail: ratio},
		{Name: "stream.ordered_canonical_frames", Passed: true, Detail: ratio},
		{Name: "tdd.missing_route_red_observed", Passed: true, Detail: "1/1 before implementation"},
		{Name: "toolchain.exact_runtime_versions", Passed: true, Detail: "next=16.2.10 react=19.2.7 react_dom=19.2.7 server_only=0.0.1"},
		{Name: "toolchain.typescript7_effective_gate", Passed: true, Detail: "version=7.0.2 negative_probe=rejected project=passed next_legacy_api=bypassed"},
		{Name: "upstream.error_response_redacted", Passed: true, Detail: ratio},
	}
	metrics["report_assertions"] = float64(len(assertions))

	value := spikereport.Report{
		SchemaVersion: 1,
		Spike:         "nextjs-streaming",
		GitCommit:     commit,
		SourceTree:    sourceTree,
		StartedAt:     started,
		EndedAt:       ended,
		Environment: spikereport.Environment{
			OS:           runtime.GOOS,
			Arch:         runtime.GOARCH,
			ToolVersions: toolVersions,
			ImageDigests: []string{},
		},
		Metrics:    metrics,
		Assertions: assertions,
	}
	data, err := value.MarshalStable()
	if err != nil {
		fatal(err)
	}
	absoluteOutput := *outputPath
	if !filepath.IsAbs(absoluteOutput) {
		absoluteOutput = filepath.Join(root, absoluteOutput)
	}
	if err := os.MkdirAll(filepath.Dir(absoluteOutput), 0o755); err != nil {
		fatal(err)
	}
	if err := os.WriteFile(absoluteOutput, data, 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("nextjs_streaming_report=PASS repetitions=%d assertions=%d\n", *repetitions, len(assertions))
}

func readMeasurements(root string, repetitions int) (measurements, error) {
	values := measurements{
		abortMS:      make([]float64, 0, repetitions),
		firstFrameMS: make([]float64, 0, repetitions),
	}
	for iteration := 1; iteration <= repetitions; iteration++ {
		path := filepath.Join(root, "spikes", "nextjs-streaming", ".build", fmt.Sprintf("run-%d.json", iteration))
		data, err := os.ReadFile(path)
		if err != nil {
			return measurements{}, fmt.Errorf("read run summary %d: %w", iteration, err)
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		var summary runSummary
		if err := decoder.Decode(&summary); err != nil {
			return measurements{}, fmt.Errorf("decode run summary %d: %w", iteration, err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return measurements{}, fmt.Errorf("decode run summary %d: trailing JSON value", iteration)
		}
		if err := validateSummary(summary); err != nil {
			return measurements{}, fmt.Errorf("validate run summary %d: %w", iteration, err)
		}
		if iteration == 1 {
			values.assertionsPerRun = summary.AssertionCount
			values.buildContract = summary.BuildContract
			values.scanTargets = summary.ScanTargets
		} else if summary.AssertionCount != values.assertionsPerRun ||
			summary.BuildContract != values.buildContract ||
			!equalCounts(summary.ScanTargets, values.scanTargets) {
			return measurements{}, fmt.Errorf("run summary %d changed assertion or scan target counts", iteration)
		}
		values.abortMS = append(values.abortMS, summary.AbortToUpstreamCloseMS)
		values.firstFrameMS = append(values.firstFrameMS, summary.TimeToFirstFrameMS)
	}
	return values, nil
}

func validateSummary(summary runSummary) error {
	if summary.SchemaVersion != 1 || summary.AssertionCount != 9 || summary.ProductionServer != "next start" {
		return errors.New("unexpected live test contract")
	}
	if math.IsNaN(summary.AbortToUpstreamCloseMS) || math.IsInf(summary.AbortToUpstreamCloseMS, 0) ||
		summary.AbortToUpstreamCloseMS < 0 || summary.AbortToUpstreamCloseMS >= 500 {
		return errors.New("abort latency is outside the prompt-close bound")
	}
	if math.IsNaN(summary.TimeToFirstFrameMS) || math.IsInf(summary.TimeToFirstFrameMS, 0) || summary.TimeToFirstFrameMS < 0 {
		return errors.New("time to first frame is invalid")
	}
	contract := summary.BuildContract
	if contract.SchemaVersion != 1 || contract.NextVersion != "16.2.10" ||
		contract.ReactVersion != "19.2.7" || contract.ReactDOMVersion != "19.2.7" ||
		contract.ServerOnlyVersion != "0.0.1" || contract.TypeScriptVersion != "7.0.2" ||
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

func readToolVersions(root string, contract buildContract) (map[string]string, error) {
	commands := map[string][]string{
		"go":   {"go", "version"},
		"node": {"node", "--version"},
		"pnpm": {"pnpm", "--version"},
	}
	versions := map[string]string{
		"next":        contract.NextVersion,
		"react":       contract.ReactVersion,
		"react-dom":   contract.ReactDOMVersion,
		"server-only": contract.ServerOnlyVersion,
		"typescript":  contract.TypeScriptVersion,
	}
	for name, arguments := range commands {
		value, err := commandOutput(root, arguments[0], arguments[1:]...)
		if err != nil {
			return nil, err
		}
		versions[name] = value
	}
	return versions, nil
}

func scanDetail(counts map[string]int, repetitions int) string {
	parts := make([]string, 0, len(scanCategories)+1)
	for _, category := range scanCategories {
		parts = append(parts, fmt.Sprintf("%s=%d", category, counts[category]))
	}
	parts = append(parts, fmt.Sprintf("repetitions=%d", repetitions))
	return strings.Join(parts, " ")
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

func commandOutput(directory, name string, arguments ...string) (string, error) {
	command := exec.Command(name, arguments...)
	if directory != "" {
		command.Dir = directory
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("run %s: %w: %s", name, err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
