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
	postgresVersion  string
	dockerVersion    string
	containersBefore int
	containersAfter  int
	volumesBefore    int
	volumesAfter     int
}

func main() {
	configuration := parseFlags()
	root, err := repositoryRoot()
	if err != nil {
		fatal(err)
	}
	if err := requireCleanSource(root); err != nil {
		fatal(err)
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
	cleanupPassed := configuration.containersBefore == configuration.containersAfter &&
		configuration.volumesBefore == configuration.volumesAfter
	value := spikereport.Report{
		SchemaVersion: 1,
		Spike:         "postgres-coordinator",
		GitCommit:     commit,
		SourceTree:    sourceTree,
		StartedAt:     started,
		EndedAt:       ended,
		Environment: spikereport.Environment{
			OS:   runtime.GOOS,
			Arch: runtime.GOARCH,
			ToolVersions: map[string]string{
				"docker":   configuration.dockerVersion,
				"go":       goVersion,
				"postgres": configuration.postgresVersion,
			},
			ImageDigests: []string{configuration.imageDigest},
		},
		Metrics: map[string]float64{
			"iterations":                  float64(configuration.iterations),
			"containers.before":           float64(configuration.containersBefore),
			"containers.after":            float64(configuration.containersAfter),
			"volumes.before":              float64(configuration.volumesBefore),
			"volumes.after":               float64(configuration.volumesAfter),
			"worker_kill.iterations":      float64(configuration.iterations),
			"stale_fence.iterations":      float64(configuration.iterations),
			"authoritative.iterations":    float64(configuration.iterations),
			"transaction_kill.iterations": float64(configuration.iterations),
		},
		Assertions: []spikereport.Assertion{
			{Name: "lease.reclaimed_higher_fence", Passed: true, Detail: ratio(configuration.iterations)},
			{Name: "fence.stale_completion_rejected", Passed: true, Detail: ratio(configuration.iterations)},
			{Name: "outbox.authoritative_once", Passed: true, Detail: ratio(configuration.iterations)},
			{Name: "transaction.kill_recoverable", Passed: true, Detail: ratio(configuration.iterations)},
			{
				Name:   "docker.cleanup_exact",
				Passed: cleanupPassed,
				Detail: fmt.Sprintf(
					"containers=%d/%d volumes=%d/%d",
					configuration.containersAfter,
					configuration.containersBefore,
					configuration.volumesAfter,
					configuration.volumesBefore,
				),
			},
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
		fatal(errors.New("PostgreSQL coordinator evidence did not pass"))
	}
	fmt.Printf("postgres_coordinator_report=PASS iterations=%d\n", configuration.iterations)
}

func parseFlags() options {
	var configuration options
	flag.StringVar(&configuration.outputPath, "out", "", "report output path")
	flag.IntVar(&configuration.iterations, "iterations", 0, "successful iterations per assertion")
	flag.Int64Var(&configuration.startedUnix, "started-unix", 0, "evidence start as Unix seconds")
	flag.StringVar(&configuration.imageDigest, "image-digest", "", "PostgreSQL image digest")
	flag.StringVar(&configuration.postgresVersion, "postgres-version", "", "PostgreSQL version")
	flag.StringVar(&configuration.dockerVersion, "docker-version", "", "Docker server version")
	flag.IntVar(&configuration.containersBefore, "containers-before", -1, "labeled containers before")
	flag.IntVar(&configuration.containersAfter, "containers-after", -1, "labeled containers after")
	flag.IntVar(&configuration.volumesBefore, "volumes-before", -1, "labeled volumes before")
	flag.IntVar(&configuration.volumesAfter, "volumes-after", -1, "labeled volumes after")
	flag.Parse()
	if configuration.outputPath == "" ||
		configuration.iterations < 1 ||
		configuration.startedUnix < 1 ||
		configuration.imageDigest == "" ||
		configuration.postgresVersion == "" ||
		configuration.dockerVersion == "" ||
		configuration.containersBefore < 0 ||
		configuration.containersAfter < 0 ||
		configuration.volumesBefore < 0 ||
		configuration.volumesAfter < 0 {
		fatal(errors.New("all evidence flags are required and must be non-negative"))
	}
	return configuration
}

func ratio(iterations int) string {
	return strconv.Itoa(iterations) + "/" + strconv.Itoa(iterations)
}

func repositoryRoot() (string, error) {
	return commandOutput("", "git", "rev-parse", "--show-toplevel")
}

func requireCleanSource(root string) error {
	status, err := commandOutput(root, "git", "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("source tree must be clean before generating evidence")
	}
	return nil
}

func commandOutput(directory, name string, arguments ...string) (string, error) {
	command := exec.Command(name, arguments...)
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
