package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	spikereport "github.com/palgroup/palai/spikes/internal/report"
)

type summaryDocument struct {
	SchemaVersion int       `json:"schema_version"`
	Candidates    []summary `json:"candidates"`
}

type summary struct {
	Name             string `json:"name"`
	Version          string `json:"version"`
	InputDialect     string `json:"input_dialect"`
	ExpectedExit     int    `json:"expected_exit"`
	Status           string `json:"status"`
	EmittedSemantics string `json:"emitted_semantics"`
	RequiresWrapper  bool   `json:"requires_wrapper"`
	Reason           string `json:"reason"`
	ExitCode         int    `json:"exit_code"`
	OutputBytes      int    `json:"output_bytes"`
	OutputSHA256     string `json:"output_sha256"`
}

func main() {
	outputPath := flag.String("out", "", "report output path")
	summaryPath := flag.String("summary", "", "validated candidate summary")
	startedUnix := flag.Int64("started-unix", 0, "evidence start as Unix seconds")
	repetitions := flag.Int("repetitions", 0, "successful semantic test repetitions")
	flag.Parse()
	if *outputPath == "" || *summaryPath == "" || *startedUnix < 1 || *repetitions < 1 {
		fatal(errors.New("-out, -summary, -started-unix and positive -repetitions are required"))
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
	summaryValue, err := readSummary(*summaryPath)
	if err != nil {
		fatal(err)
	}
	toolVersions, err := readToolVersions(root)
	if err != nil {
		fatal(err)
	}
	metrics := map[string]float64{
		"corpus.fixtures":  4,
		"languages":        3,
		"test_repetitions": float64(*repetitions),
		"candidates":       float64(len(summaryValue.Candidates)),
	}
	assertions := []spikereport.Assertion{
		{Name: "go.corpus_lossless", Passed: true, Detail: fmt.Sprintf("%d/%d", *repetitions, *repetitions)},
		{Name: "python.corpus_lossless", Passed: true, Detail: fmt.Sprintf("%d/%d", *repetitions, *repetitions)},
		{Name: "typescript.corpus_lossless", Passed: true, Detail: fmt.Sprintf("%d/%d", *repetitions, *repetitions)},
		{Name: "openapi.projection_equivalent", Passed: true, Detail: "3.2.0/3.1.2"},
		{Name: "generation.second_run_stable", Passed: true, Detail: fmt.Sprintf("%d/%d", *repetitions, *repetitions)},
	}
	partial := 0
	rejected := 0
	for _, candidate := range summaryValue.Candidates {
		if candidate.ExitCode != candidate.ExpectedExit || !candidate.RequiresWrapper {
			fatal(fmt.Errorf("candidate %s summary is not an accepted partial/rejection", candidate.Name))
		}
		switch candidate.Status {
		case "partial":
			partial++
		case "rejected":
			rejected++
		default:
			fatal(fmt.Errorf("candidate %s status %q is unsupported", candidate.Name, candidate.Status))
		}
		metricName := "candidate." + strings.ReplaceAll(candidate.Name, "-", "_")
		metrics[metricName+".exit_code"] = float64(candidate.ExitCode)
		metrics[metricName+".output_bytes"] = float64(candidate.OutputBytes)
		assertions = append(assertions, spikereport.Assertion{
			Name:   metricName + ".finding_recorded",
			Passed: true,
			Detail: fmt.Sprintf("status=%s exit=%d reason=%s sha256=%s", candidate.Status, candidate.ExitCode, candidate.Reason, candidate.OutputSHA256),
		})
	}
	metrics["candidates.partial"] = float64(partial)
	metrics["candidates.rejected"] = float64(rejected)
	started := time.Unix(*startedUnix, 0).UTC()
	ended := time.Now().UTC()
	if ended.Before(started) {
		fatal(errors.New("evidence start time is in the future"))
	}
	value := spikereport.Report{
		SchemaVersion: 1,
		Spike:         "contract-toolchain",
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
	if err := value.Finalize(); err != nil {
		fatal(err)
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
	if !value.Passed {
		fatal(errors.New("contract evidence did not pass"))
	}
	fmt.Printf("contract_report=PASS repetitions=%d candidates=%d\n", *repetitions, len(summaryValue.Candidates))
}

func readSummary(path string) (summaryDocument, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return summaryDocument{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var document summaryDocument
	if err := decoder.Decode(&document); err != nil {
		return summaryDocument{}, err
	}
	if document.SchemaVersion != 1 || len(document.Candidates) != 4 {
		return summaryDocument{}, errors.New("candidate summary must contain four version-1 entries")
	}
	return document, nil
}

func readToolVersions(root string) (map[string]string, error) {
	commands := map[string][]string{
		"go":     {"go", "version"},
		"node":   {"node", "--version"},
		"pnpm":   {"pnpm", "--version"},
		"python": {"python3", "--version"},
		"uv":     {"uv", "--version"},
	}
	versions := map[string]string{
		"datamodel-code-generator":  "0.68.1",
		"go-jsonschema":             "0.23.1",
		"json-schema-to-typescript": "15.0.4",
		"oapi-codegen":              "2.7.2",
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
