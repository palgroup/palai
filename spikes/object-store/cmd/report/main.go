package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	spikereport "github.com/palgroup/palai/spikes/internal/report"
	objectstore "github.com/palgroup/palai/spikes/object-store"
)

var reportDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type options struct {
	outputPath       string
	runDirectory     string
	registryPath     string
	archivePath      string
	repetitions      int
	startedUnix      int64
	indexDigest      string
	platformDigest   string
	imageID          string
	platform         string
	dockerVersion    string
	containersBefore int
	containersAfter  int
	volumesBefore    int
	volumesAfter     int
	secretScan       bool
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
	tree, err := commandOutput(root, "git", "rev-parse", "HEAD^{tree}")
	if err != nil {
		fatal(err)
	}

	catalogData, err := os.ReadFile(filepath.Join(root, "spikes/object-store/candidates.json"))
	if err != nil {
		fatal(errors.New("read candidate catalog"))
	}
	catalog, err := objectstore.DecodeCatalog(bytes.NewReader(catalogData))
	if err != nil {
		fatal(err)
	}
	evaluation, err := objectstore.EvaluateCatalog(catalog)
	if err != nil {
		fatal(err)
	}
	values, err := readMeasurements(configuration.runDirectory, configuration.repetitions, commit, tree)
	if err != nil {
		fatal(err)
	}
	registryData, err := os.ReadFile(configuration.registryPath)
	if err != nil {
		fatal(errors.New("read registry evidence"))
	}
	registry, err := decodeRegistryEvidence(registryData, catalog)
	if err != nil {
		fatal(err)
	}
	archiveData, err := os.ReadFile(configuration.archivePath)
	if err != nil {
		fatal(errors.New("read archive evidence"))
	}
	archive, err := decodeArchiveEvidence(archiveData, configuration.imageID)
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
	cleanup := cleanupEvidence{
		containersBefore: configuration.containersBefore,
		containersAfter:  configuration.containersAfter,
		volumesBefore:    configuration.volumesBefore,
		volumesAfter:     configuration.volumesAfter,
	}
	metrics := latencyMetrics(values)
	metrics["archive.bytes"] = float64(archive.archiveBytes)
	metrics["bytes_verified.total"] = float64(values.bytesVerified)
	metrics["containers.after"] = float64(configuration.containersAfter)
	metrics["containers.before"] = float64(configuration.containersBefore)
	metrics["live_cases.per_repetition"] = float64(len(expectedConformanceCases) + len(expectedPersistenceCases))
	metrics["live_cases.total"] = float64((len(expectedConformanceCases) + len(expectedPersistenceCases)) * configuration.repetitions)
	metrics["registry.candidates"] = float64(registry.exactCandidates)
	metrics["test_repetitions"] = float64(configuration.repetitions)
	metrics["volumes.after"] = float64(configuration.volumesAfter)
	metrics["volumes.before"] = float64(configuration.volumesBefore)

	report := spikereport.Report{
		SchemaVersion: 1,
		Spike:         "object-store",
		GitCommit:     commit,
		SourceTree:    tree,
		StartedAt:     started,
		EndedAt:       ended,
		Environment: spikereport.Environment{
			OS:   runtime.GOOS,
			Arch: runtime.GOARCH,
			ToolVersions: map[string]string{
				"aws-sdk-s3":      "1.105.1",
				"docker":          configuration.dockerVersion,
				"docker-platform": configuration.platform,
				"go":              goVersion,
				"seaweedfs":       "4.39",
			},
			ImageDigests: []string{
				configuration.indexDigest,
				configuration.platformDigest,
				configuration.imageID,
			},
		},
		Metrics: metrics,
		Assertions: deriveAssertions(
			values,
			registry,
			archive,
			evaluation,
			cleanup,
			configuration.secretScan,
			configuration.repetitions,
		),
	}
	if err := report.Finalize(); err != nil {
		fatal(err)
	}
	data, err := report.MarshalStable()
	if err != nil {
		fatal(err)
	}
	outputPath := configuration.outputPath
	if !filepath.IsAbs(outputPath) {
		outputPath = filepath.Join(root, outputPath)
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		fatal(errors.New("create report directory"))
	}
	if err := os.WriteFile(outputPath, data, 0o644); err != nil {
		fatal(errors.New("write object-store report"))
	}
	if !report.Passed {
		fatal(errors.New("object-store evidence did not pass"))
	}
	fmt.Printf("object_store_report=PASS repetitions=%d assertions=%d\n", configuration.repetitions, len(report.Assertions))
}

func parseFlags() options {
	var configuration options
	flag.StringVar(&configuration.outputPath, "out", "", "report output path")
	flag.StringVar(&configuration.runDirectory, "run-dir", "", "run evidence directory")
	flag.StringVar(&configuration.registryPath, "registry", "", "live registry summary path")
	flag.StringVar(&configuration.archivePath, "archive", "", "local archive round-trip summary path")
	flag.IntVar(&configuration.repetitions, "repetitions", 0, "successful live repetitions")
	flag.Int64Var(&configuration.startedUnix, "started-unix", 0, "evidence start as Unix seconds")
	flag.StringVar(&configuration.indexDigest, "index-digest", "", "selected OCI index digest")
	flag.StringVar(&configuration.platformDigest, "platform-digest", "", "selected host platform manifest digest")
	flag.StringVar(&configuration.imageID, "image-id", "", "selected local image config ID")
	flag.StringVar(&configuration.platform, "platform", "", "selected Linux platform")
	flag.StringVar(&configuration.dockerVersion, "docker-version", "", "Docker server version")
	flag.IntVar(&configuration.containersBefore, "containers-before", -1, "labeled containers before")
	flag.IntVar(&configuration.containersAfter, "containers-after", -1, "labeled containers after")
	flag.IntVar(&configuration.volumesBefore, "volumes-before", -1, "labeled volumes before")
	flag.IntVar(&configuration.volumesAfter, "volumes-after", -1, "labeled volumes after")
	flag.BoolVar(&configuration.secretScan, "secret-scan", false, "exact credential sentinel scan passed")
	flag.Parse()
	if configuration.outputPath == "" || configuration.runDirectory == "" ||
		configuration.registryPath == "" || configuration.archivePath == "" ||
		configuration.repetitions < 1 || configuration.startedUnix < 1 ||
		!reportDigestPattern.MatchString(configuration.indexDigest) ||
		!reportDigestPattern.MatchString(configuration.platformDigest) ||
		!reportDigestPattern.MatchString(configuration.imageID) ||
		(configuration.platform != "linux/amd64" && configuration.platform != "linux/arm64") ||
		strings.TrimSpace(configuration.dockerVersion) == "" ||
		configuration.containersBefore < 0 || configuration.containersAfter < 0 ||
		configuration.volumesBefore < 0 || configuration.volumesAfter < 0 {
		fatal(errors.New("all object-store evidence flags are required and must be valid"))
	}
	return configuration
}

func commandOutput(directory, name string, arguments ...string) (string, error) {
	command := exec.Command(name, arguments...)
	if directory != "" {
		command.Dir = directory
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("run %s: %w", name, err)
	}
	return strings.TrimSpace(string(output)), nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
