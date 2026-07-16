package main

import (
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

func main() {
	outputPath := flag.String("out", "", "report output path")
	repetitions := flag.Int("repetitions", 0, "successful live production-server repetitions")
	runDirectory := flag.String("run-dir", "", "invocation-specific run summary directory")
	runID := flag.String("invocation-id", "", "invocation identifier bound to every run summary")
	startedUnix := flag.Int64("started-unix", 0, "evidence start as Unix seconds")
	flag.Parse()
	if *outputPath == "" || *repetitions < 1 || *runDirectory == "" || *runID == "" || *startedUnix < 1 {
		fatal(errors.New("-out, -run-dir, -invocation-id, positive -repetitions and -started-unix are required"))
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

	absoluteRunDirectory := *runDirectory
	if !filepath.IsAbs(absoluteRunDirectory) {
		absoluteRunDirectory = filepath.Join(root, absoluteRunDirectory)
	}
	values, err := readMeasurements(
		absoluteRunDirectory,
		*repetitions,
		commit,
		sourceTree,
		*runID,
	)
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

	assertions := deriveAssertions(values.outcomeCounts, *repetitions)
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
