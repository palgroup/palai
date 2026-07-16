package report

import (
	"bytes"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	testCommit = "0123456789abcdef0123456789abcdef01234567"
	testTree   = "89abcdef0123456789abcdef0123456789abcdef"
)

func validReport() Report {
	started := time.Date(2026, time.July, 16, 10, 0, 0, 0, time.UTC)
	return Report{
		SchemaVersion: 1,
		Spike:         "control-plane-runtime",
		GitCommit:     testCommit,
		SourceTree:    testTree,
		StartedAt:     started,
		EndedAt:       started.Add(time.Second),
		Environment: Environment{
			OS:   "darwin",
			Arch: "arm64",
			ToolVersions: map[string]string{
				"node": "v22.22.2",
				"go":   "go1.26.4",
			},
			ImageDigests: []string{
				"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			},
		},
		Metrics: map[string]float64{
			"node.idle_rss_bytes": 2,
			"go.idle_rss_bytes":   1,
		},
		Assertions: []Assertion{
			{Name: "z.last", Passed: true, Detail: "1/1"},
			{Name: "a.first", Passed: true, Detail: "1/1"},
		},
	}
}

func TestFinalizeDerivesPassFromEveryAssertion(t *testing.T) {
	report := validReport()
	report.Passed = false

	if err := report.Finalize(); err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}
	if !report.Passed {
		t.Fatal("Finalize() did not derive passed=true")
	}

	report.Assertions[0].Passed = false
	report.Passed = true
	if err := report.Finalize(); err != nil {
		t.Fatalf("Finalize() with failed assertion error = %v", err)
	}
	if report.Passed {
		t.Fatal("Finalize() accepted a failed assertion")
	}
}

func TestValidateRejectsMissingCommitAndUnboundedMetric(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Report)
		want   string
	}{
		{
			name: "missing commit",
			mutate: func(report *Report) {
				report.GitCommit = ""
			},
			want: "git_commit",
		},
		{
			name: "missing source tree",
			mutate: func(report *Report) {
				report.SourceTree = ""
			},
			want: "source_tree",
		},
		{
			name: "infinite metric",
			mutate: func(report *Report) {
				report.Metrics["go.idle_rss_bytes"] = math.Inf(1)
			},
			want: "finite non-negative",
		},
		{
			name: "negative metric",
			mutate: func(report *Report) {
				report.Metrics["go.idle_rss_bytes"] = -1
			},
			want: "finite non-negative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := validReport()
			tt.mutate(&report)
			err := report.Finalize()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Finalize() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestMarshalStableOrdersCollectionsAndOmitsHostIdentity(t *testing.T) {
	report := validReport()
	if err := report.Finalize(); err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}

	first, err := report.MarshalStable()
	if err != nil {
		t.Fatalf("MarshalStable() error = %v", err)
	}
	second, err := report.MarshalStable()
	if err != nil {
		t.Fatalf("second MarshalStable() error = %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("MarshalStable() changed output:\n%s\n%s", first, second)
	}

	text := string(first)
	if strings.Index(text, "a.first") > strings.Index(text, "z.last") {
		t.Fatalf("assertions are not sorted: %s", text)
	}
	if strings.Index(text, "sha256:aaaa") > strings.Index(text, "sha256:bbbb") {
		t.Fatalf("image digests are not sorted: %s", text)
	}
	for _, forbidden := range []string{"hostname", "username", "/Users/", "/home/runner"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("output contains host identity marker %q", forbidden)
		}
	}
}

func TestReportRejectsSecretLikeValues(t *testing.T) {
	markers := []string{
		"OPENAI_API_KEY=sentinel",
		"ANTROPHIC_API_KEY=sentinel",
		"Authorization: Bearer sentinel",
		"sk-test-sentinel",
		"-----BEGIN PRIVATE KEY-----",
		"/Users/example/private/path",
		"/home/runner/work/private/path",
	}

	for _, marker := range markers {
		t.Run(marker, func(t *testing.T) {
			report := validReport()
			report.Assertions[0].Detail = marker
			err := report.Finalize()
			if err == nil || !strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("Finalize() error = %v, want sensitive-value rejection", err)
			}
		})
	}
}

func TestDecodeRejectsUnknownFieldsAndStoredPassMismatch(t *testing.T) {
	data, err := os.ReadFile("testdata/valid.json")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	report, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if err := report.ValidateStored(); err != nil {
		t.Fatalf("ValidateStored() error = %v", err)
	}

	report.Passed = false
	if err := report.ValidateStored(); err == nil || !strings.Contains(err.Error(), "passed") {
		t.Fatalf("ValidateStored() error = %v, want passed mismatch", err)
	}

	trimmed := bytes.TrimSpace(data)
	unknown := append([]byte(nil), trimmed[:len(trimmed)-1]...)
	unknown = append(unknown, []byte(",\n  \"host\": \"hidden\"\n}\n")...)
	if _, err := Decode(unknown); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("Decode() error = %v, want unknown field", err)
	}
}
