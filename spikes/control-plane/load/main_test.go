package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/spikes/internal/report"
)

func TestWriteReportRejectsFailedEvidenceAfterPersistingIt(t *testing.T) {
	started := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	value := report.Report{
		SchemaVersion: 1,
		Spike:         "control-plane-runtime",
		GitCommit:     "0123456789abcdef0123456789abcdef01234567",
		SourceTree:    "89abcdef0123456789abcdef0123456789abcdef",
		StartedAt:     started,
		EndedAt:       started.Add(time.Second),
		Environment: report.Environment{
			OS:           "linux",
			Arch:         "amd64",
			ToolVersions: map[string]string{"go": "go1.26.4"},
			ImageDigests: []string{},
		},
		Metrics: map[string]float64{"go.connections": 999},
		Assertions: []report.Assertion{
			{Name: "go.connections_exact", Passed: false, Detail: "999/1000"},
		},
	}
	path := filepath.Join(t.TempDir(), "nested", "report.json")
	err := writeReport(path, value)
	if err == nil || !strings.Contains(err.Error(), "did not pass") {
		t.Fatalf("writeReport() error = %v, want failed-evidence rejection", err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile() error = %v", readErr)
	}
	stored, decodeErr := report.Decode(data)
	if decodeErr != nil {
		t.Fatalf("Decode() error = %v", decodeErr)
	}
	if validateErr := stored.ValidateStored(); validateErr != nil {
		t.Fatalf("ValidateStored() error = %v", validateErr)
	}
	if stored.Passed {
		t.Fatal("stored failed evidence has passed=true")
	}

	value.Assertions[0].Passed = true
	value.Assertions[0].Detail = "1000/1000"
	passingPath := filepath.Join(t.TempDir(), "passing.json")
	if err := writeReport(passingPath, value); err != nil {
		t.Fatalf("writeReport() passing error = %v", err)
	}
	passingData, err := os.ReadFile(passingPath)
	if err != nil {
		t.Fatalf("ReadFile() passing error = %v", err)
	}
	passing, err := report.Decode(passingData)
	if err != nil {
		t.Fatalf("Decode() passing error = %v", err)
	}
	if !passing.Passed {
		t.Fatal("stored passing evidence has passed=false")
	}
}
