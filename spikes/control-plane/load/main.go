package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/palgroup/palai/spikes/internal/report"
)

const (
	maximumGoIdleRSS   = 128 * 1024 * 1024
	maximumNodeIdleRSS = 256 * 1024 * 1024
)

func main() {
	profileName := flag.String("profile", "", "quick or evidence")
	outputPath := flag.String("out", "", "report output path")
	flag.Parse()
	if *outputPath == "" {
		fatal(errors.New("-out is required"))
	}
	var profile loadProfile
	switch *profileName {
	case "quick":
		profile = quickLoadProfile()
	case "evidence":
		profile = evidenceLoadProfile()
	default:
		fatal(errors.New("-profile must be quick or evidence"))
	}

	root, err := repositoryRoot()
	if err != nil {
		fatal(err)
	}
	if err := requireCleanSource(root); err != nil {
		fatal(err)
	}
	commit, sourceTree, err := gitIdentity(root)
	if err != nil {
		fatal(err)
	}
	started := time.Now().UTC()
	candidates, err := prepareCandidates(context.Background(), root)
	if err != nil {
		fatal(err)
	}
	results := make([]candidateResult, 0, len(candidates))
	for _, candidate := range candidates {
		result, err := runCandidateLoad(context.Background(), candidate, profile)
		if err != nil {
			fatal(fmt.Errorf("run %s candidate: %w", candidate.Name, err))
		}
		results = append(results, result)
		fmt.Fprintf(os.Stderr, "candidate=%s connections=%d reconnects=%d rss=%d\n", result.Name, result.Connected, result.Reconnects, result.ConnectedRSSBytes)
	}
	reportValue, err := buildReport(started, time.Now().UTC(), commit, sourceTree, profile, results)
	if err != nil {
		fatal(err)
	}
	absoluteOutput := *outputPath
	if !filepath.IsAbs(absoluteOutput) {
		absoluteOutput = filepath.Join(root, absoluteOutput)
	}
	if err := writeReport(absoluteOutput, reportValue); err != nil {
		fatal(err)
	}
	fmt.Printf("control_plane_runtime=PASS profile=%s candidates=%d\n", *profileName, len(results))
}

func writeReport(path string, value report.Report) error {
	if err := value.Finalize(); err != nil {
		return err
	}
	data, err := value.MarshalStable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create report directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	if !value.Passed {
		return errors.New("control-plane evidence did not pass")
	}
	return nil
}

func buildReport(
	started time.Time,
	ended time.Time,
	commit string,
	sourceTree string,
	profile loadProfile,
	results []candidateResult,
) (report.Report, error) {
	toolVersions, err := runtimeToolVersions()
	if err != nil {
		return report.Report{}, err
	}
	metrics := make(map[string]float64)
	assertions := make([]report.Assertion, 0, len(results)*7)
	for _, result := range results {
		prefix := result.Name
		metrics[prefix+".ready_milliseconds"] = float64(result.ReadyDuration.Milliseconds())
		metrics[prefix+".idle_rss_bytes"] = float64(result.IdleRSSBytes)
		metrics[prefix+".connected_rss_bytes"] = float64(result.ConnectedRSSBytes)
		metrics[prefix+".connections"] = float64(result.Connected)
		metrics[prefix+".reconnects"] = float64(result.Reconnects)
		metrics[prefix+".restart_cycles"] = float64(result.RestartCycles)
		metrics[prefix+".sequence_duplicates"] = float64(result.SequenceDuplicates)
		metrics[prefix+".sequence_gaps"] = float64(result.SequenceGaps)
		metrics[prefix+".errors"] = float64(result.Errors)
		metrics[prefix+".cancel_requests"] = float64(result.CancelRequests)
		metrics[prefix+".shutdown_milliseconds"] = float64(result.ShutdownDuration.Milliseconds())
		assertions = append(assertions,
			report.Assertion{
				Name:   prefix + ".connections_exact",
				Passed: result.Connected == profile.Connections,
				Detail: fmt.Sprintf("%d/%d", result.Connected, profile.Connections),
			},
			report.Assertion{
				Name:   prefix + ".reconnects_exact",
				Passed: result.Reconnects == profile.Reconnects,
				Detail: fmt.Sprintf("%d/%d", result.Reconnects, profile.Reconnects),
			},
			report.Assertion{
				Name:   prefix + ".restart_exact",
				Passed: result.RestartCycles == profile.RestartCycles,
				Detail: fmt.Sprintf("%d/%d", result.RestartCycles, profile.RestartCycles),
			},
			report.Assertion{
				Name:   prefix + ".sequence_exact",
				Passed: result.SequenceDuplicates == 0 && result.SequenceGaps == 0 && result.Errors <= profile.MaximumErrors,
				Detail: fmt.Sprintf("duplicates=%d gaps=%d errors=%d limit=%d", result.SequenceDuplicates, result.SequenceGaps, result.Errors, profile.MaximumErrors),
			},
			report.Assertion{
				Name:   prefix + ".disconnect_not_cancel",
				Passed: result.CancelRequests == 0,
				Detail: fmt.Sprintf("cancel_requests=%d", result.CancelRequests),
			},
			report.Assertion{
				Name:   prefix + ".shutdown_bound",
				Passed: result.ShutdownDuration <= profile.ShutdownDeadline,
				Detail: fmt.Sprintf("milliseconds=%d limit=%d", result.ShutdownDuration.Milliseconds(), profile.ShutdownDeadline.Milliseconds()),
			},
			report.Assertion{
				Name:   prefix + ".idle_rss_bound",
				Passed: result.IdleRSSBytes <= rssLimit(result.Name),
				Detail: fmt.Sprintf("bytes=%d limit=%d", result.IdleRSSBytes, rssLimit(result.Name)),
			},
		)
	}
	return report.Report{
		SchemaVersion: 1,
		Spike:         "control-plane-runtime",
		GitCommit:     commit,
		SourceTree:    sourceTree,
		StartedAt:     started,
		EndedAt:       ended,
		Environment: report.Environment{
			OS:           runtime.GOOS,
			Arch:         runtime.GOARCH,
			ToolVersions: toolVersions,
			ImageDigests: []string{},
		},
		Metrics:    metrics,
		Assertions: assertions,
	}, nil
}

func rssLimit(candidate string) int64 {
	if candidate == "node" {
		return maximumNodeIdleRSS
	}
	return maximumGoIdleRSS
}

func requireCleanSource(root string) error {
	command := exec.Command("git", "status", "--porcelain", "--untracked-files=all")
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("inspect source status: %w", err)
	}
	if strings.TrimSpace(string(output)) != "" {
		return errors.New("source tree must be clean before generating evidence")
	}
	return nil
}

func gitIdentity(root string) (string, string, error) {
	commit, err := commandOutput(root, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	sourceTree, err := commandOutput(root, "git", "rev-parse", "HEAD^{tree}")
	if err != nil {
		return "", "", err
	}
	return commit, sourceTree, nil
}

func runtimeToolVersions() (map[string]string, error) {
	goVersion, err := commandOutput("", "go", "version")
	if err != nil {
		return nil, err
	}
	nodeVersion, err := commandOutput("", "node", "--version")
	if err != nil {
		return nil, err
	}
	return map[string]string{"go": goVersion, "node": nodeVersion}, nil
}

func commandOutput(directory, name string, args ...string) (string, error) {
	command := exec.Command(name, args...)
	if directory != "" {
		command.Dir = directory
	}
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("run %s: %w", name, err)
	}
	return strings.TrimSpace(string(output)), nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
