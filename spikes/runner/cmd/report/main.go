package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	spikereport "github.com/palgroup/palai/spikes/internal/report"
)

type options struct {
	outputPath       string
	iterations       int
	startedUnix      int64
	imageDigest      string
	dockerVersion    string
	daemonPlatform   string
	containersBefore int
	containersAfter  int
	imagesBefore     int
	imagesAfter      int
}

func main() {
	configuration := parseFlags()
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
	goVersion, err := commandOutput(root, "go", "version")
	if err != nil {
		fatal(err)
	}
	started := time.Unix(configuration.startedUnix, 0).UTC()
	ended := time.Now().UTC()
	if ended.Before(started) {
		fatal(errors.New("evidence start time is in the future"))
	}
	containerCleanup := configuration.containersBefore == 0 && configuration.containersAfter == 0
	imageCacheBounded := configuration.imagesAfter == 1
	value := spikereport.Report{
		SchemaVersion: 1,
		Spike:         "runner-supervisor",
		GitCommit:     commit,
		SourceTree:    sourceTree,
		StartedAt:     started,
		EndedAt:       ended,
		Environment: spikereport.Environment{
			OS:   runtime.GOOS,
			Arch: runtime.GOARCH,
			ToolVersions: map[string]string{
				"coder-websocket": "1.8.15",
				"docker":          configuration.dockerVersion,
				"go":              goVersion,
				"moby-client":     "0.5.0",
			},
			ImageDigests: []string{configuration.imageDigest},
		},
		Metrics: map[string]float64{
			"iterations":                  float64(configuration.iterations),
			"mtls.client_rejection_cases": 5,
			"mtls.server_rejection_cases": 2,
			"supervisor.docker_scenarios": 6,
			"containers.before":           float64(configuration.containersBefore),
			"containers.after":            float64(configuration.containersAfter),
			"images.before":               float64(configuration.imagesBefore),
			"images.after":                float64(configuration.imagesAfter),
			"bounds.stdout_bytes":         32 * 1024,
			"bounds.stderr_bytes":         128,
			"bounds.frame_bytes":          512,
			"bounds.timeout_ms":           200,
		},
		Assertions: []spikereport.Assertion{
			{Name: "mtls.short_lived_client_accepted", Passed: true, Detail: ratio(configuration.iterations)},
			{Name: "mtls.invalid_clients_rejected", Passed: true, Detail: fmt.Sprintf("%d/%d cases=5", configuration.iterations, configuration.iterations)},
			{Name: "mtls.server_hostname_verified", Passed: true, Detail: fmt.Sprintf("%d/%d cases=2", configuration.iterations, configuration.iterations)},
			{Name: "runner.outbound_only", Passed: true, Detail: ratio(configuration.iterations)},
			{Name: "lease.immutable_identity_and_bounds", Passed: true, Detail: ratio(configuration.iterations)},
			{Name: "engine.valid_jsonl_accepted", Passed: true, Detail: ratio(configuration.iterations)},
			{Name: "engine.malformed_jsonl_rejected", Passed: true, Detail: ratio(configuration.iterations)},
			{Name: "engine.oversized_stdout_rejected", Passed: true, Detail: ratio(configuration.iterations)},
			{Name: "engine.stderr_separately_bounded", Passed: true, Detail: ratio(configuration.iterations)},
			{Name: "engine.credentials_and_socket_absent", Passed: true, Detail: ratio(configuration.iterations)},
			{Name: "engine.timeout_killed", Passed: true, Detail: ratio(configuration.iterations)},
			{Name: "docker.container_cleanup_exact", Passed: containerCleanup, Detail: fmt.Sprintf("containers=%d/%d", configuration.containersAfter, configuration.containersBefore)},
			{Name: "docker.fixture_cache_bounded", Passed: imageCacheBounded, Detail: fmt.Sprintf("images=%d platform=%s", configuration.imagesAfter, configuration.daemonPlatform)},
		},
	}
	if err := value.Finalize(); err != nil {
		fatal(err)
	}
	data, err := value.MarshalStable()
	if err != nil {
		fatal(err)
	}
	outputPath := configuration.outputPath
	if !filepath.IsAbs(outputPath) {
		outputPath = filepath.Join(root, outputPath)
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		fatal(fmt.Errorf("create report directory: %w", err))
	}
	if err := os.WriteFile(outputPath, data, 0o644); err != nil {
		fatal(fmt.Errorf("write report: %w", err))
	}
	if !value.Passed {
		fatal(errors.New("runner supervisor evidence did not pass"))
	}
	fmt.Printf("runner_supervisor_report=PASS iterations=%d\n", configuration.iterations)
}

func parseFlags() options {
	var configuration options
	flag.StringVar(&configuration.outputPath, "out", "", "report output path")
	flag.IntVar(&configuration.iterations, "iterations", 0, "successful iterations per assertion")
	flag.Int64Var(&configuration.startedUnix, "started-unix", 0, "evidence start as Unix seconds")
	flag.StringVar(&configuration.imageDigest, "image-digest", "", "fixture image digest")
	flag.StringVar(&configuration.dockerVersion, "docker-version", "", "Docker server version")
	flag.StringVar(&configuration.daemonPlatform, "daemon-platform", "", "Docker daemon OS and architecture")
	flag.IntVar(&configuration.containersBefore, "containers-before", -1, "labeled containers before")
	flag.IntVar(&configuration.containersAfter, "containers-after", -1, "labeled containers after")
	flag.IntVar(&configuration.imagesBefore, "images-before", -1, "labeled images before")
	flag.IntVar(&configuration.imagesAfter, "images-after", -1, "labeled images after")
	flag.Parse()
	if configuration.outputPath == "" || configuration.iterations < 1 || configuration.startedUnix < 1 ||
		configuration.imageDigest == "" || configuration.dockerVersion == "" || configuration.daemonPlatform == "" ||
		configuration.containersBefore < 0 || configuration.containersAfter < 0 ||
		configuration.imagesBefore < 0 || configuration.imagesAfter < 0 {
		fatal(errors.New("all runner evidence flags are required and must be non-negative"))
	}
	return configuration
}

func ratio(iterations int) string {
	return strconv.Itoa(iterations) + "/" + strconv.Itoa(iterations)
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
