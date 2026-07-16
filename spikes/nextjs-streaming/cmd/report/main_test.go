package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testRunID = "run-12345678"

var testOutcomeNames = []string{
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

func TestDecodeRunSummaryAcceptsCompleteObservedRun(t *testing.T) {
	commit := strings.Repeat("a", 40)
	tree := strings.Repeat("b", 40)
	data := marshalRunSummary(t, validRunSummary(commit, tree, testRunID, 1))

	summary, err := decodeRunSummary(data, commit, tree, testRunID, 1)
	if err != nil {
		t.Fatalf("decode valid run summary: %v", err)
	}
	if len(summary.Outcomes) != len(testOutcomeNames) {
		t.Fatalf("outcome count = %d, want %d", len(summary.Outcomes), len(testOutcomeNames))
	}
}

func TestDecodeRunSummaryRejectsMutatedEvidence(t *testing.T) {
	commit := strings.Repeat("a", 40)
	tree := strings.Repeat("b", 40)
	tests := map[string]func(map[string]any){
		"missing outcome": func(value map[string]any) {
			outcomes := value["outcomes"].([]map[string]any)
			value["outcomes"] = outcomes[:len(outcomes)-1]
		},
		"false outcome": func(value map[string]any) {
			value["outcomes"].([]map[string]any)[0]["passed"] = false
		},
		"unknown outcome with unchanged count": func(value map[string]any) {
			value["outcomes"].([]map[string]any)[0]["name"] = "unknown.observation"
		},
		"duplicate outcome": func(value map[string]any) {
			outcomes := value["outcomes"].([]map[string]any)
			outcomes[1]["name"] = outcomes[0]["name"]
		},
		"omitted passed value": func(value map[string]any) {
			delete(value["outcomes"].([]map[string]any)[0], "passed")
		},
		"commit mismatch": func(value map[string]any) {
			value["git_commit"] = strings.Repeat("d", 40)
		},
		"tree mismatch": func(value map[string]any) {
			value["source_tree"] = strings.Repeat("d", 40)
		},
		"invocation mismatch": func(value map[string]any) {
			value["invocation_id"] = "stale-run"
		},
		"iteration mismatch": func(value map[string]any) {
			value["iteration"] = 2
		},
		"incomplete process": func(value map[string]any) {
			value["process_result"].(map[string]any)["completed"] = false
		},
		"omitted process result": func(value map[string]any) {
			delete(value, "process_result")
		},
		"omitted process completion": func(value map[string]any) {
			delete(value["process_result"].(map[string]any), "completed")
		},
		"omitted process exit code": func(value map[string]any) {
			delete(value["process_result"].(map[string]any), "exit_code")
		},
		"failed process": func(value map[string]any) {
			value["process_result"].(map[string]any)["exit_code"] = 1
		},
		"omitted abort latency": func(value map[string]any) {
			delete(value, "abort_to_upstream_close_ms")
		},
		"null abort latency": func(value map[string]any) {
			value["abort_to_upstream_close_ms"] = nil
		},
		"wrong abort latency type": func(value map[string]any) {
			value["abort_to_upstream_close_ms"] = "4.5"
		},
		"negative abort latency": func(value map[string]any) {
			value["abort_to_upstream_close_ms"] = -0.001
		},
		"abort latency at bound": func(value map[string]any) {
			value["abort_to_upstream_close_ms"] = 500.0
		},
		"omitted first frame latency": func(value map[string]any) {
			delete(value, "time_to_first_frame_ms")
		},
		"null first frame latency": func(value map[string]any) {
			value["time_to_first_frame_ms"] = nil
		},
		"wrong first frame latency type": func(value map[string]any) {
			value["time_to_first_frame_ms"] = "100.25"
		},
		"negative first frame latency": func(value map[string]any) {
			value["time_to_first_frame_ms"] = -0.001
		},
		"unknown field": func(value map[string]any) {
			value["manufactured"] = true
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			value := validRunSummary(commit, tree, testRunID, 1)
			mutate(value)
			if _, err := decodeRunSummary(
				marshalRunSummary(t, value),
				commit,
				tree,
				testRunID,
				1,
			); err == nil {
				t.Fatal("mutated run summary was accepted")
			}
		})
	}
}

func TestDecodeRunSummaryRequiresFiniteLatencies(t *testing.T) {
	commit := strings.Repeat("a", 40)
	tree := strings.Repeat("b", 40)
	valid := string(marshalRunSummary(t, validRunSummary(commit, tree, testRunID, 1)))
	for _, nonFinite := range []string{"NaN", "1e10000"} {
		t.Run(nonFinite, func(t *testing.T) {
			mutated := strings.Replace(valid, "4.5", nonFinite, 1)
			if _, err := decodeRunSummary(
				[]byte(mutated),
				commit,
				tree,
				testRunID,
				1,
			); err == nil {
				t.Fatal("non-finite latency was accepted")
			}
		})
	}
}

func TestDecodeRunSummaryAcceptsLatencyBoundaries(t *testing.T) {
	commit := strings.Repeat("a", 40)
	tree := strings.Repeat("b", 40)
	value := validRunSummary(commit, tree, testRunID, 1)
	value["abort_to_upstream_close_ms"] = 499.999
	value["time_to_first_frame_ms"] = 0.0
	if _, err := decodeRunSummary(
		marshalRunSummary(t, value),
		commit,
		tree,
		testRunID,
		1,
	); err != nil {
		t.Fatalf("decode boundary latencies: %v", err)
	}
}

func TestDeriveAssertionsUsesObservedOutcomeCounts(t *testing.T) {
	counts := make(map[string]int, len(testOutcomeNames))
	for _, name := range testOutcomeNames {
		counts[name] = 2
	}
	counts["abort.explicit_cancel_not_called"] = 1

	assertions := deriveAssertions(counts, 2)
	if len(assertions) != len(testOutcomeNames) {
		t.Fatalf("assertion count = %d, want %d", len(assertions), len(testOutcomeNames))
	}
	for _, assertion := range assertions {
		if assertion.Name == "tdd.missing_route_red_observed" {
			t.Fatal("unsupported TDD receipt was emitted")
		}
		if assertion.Name == "abort.explicit_cancel_not_called" {
			if assertion.Passed {
				t.Fatal("partially observed outcome was relabeled as passed")
			}
			if assertion.Detail != "1/2 observed" {
				t.Fatalf("partial observation detail = %q", assertion.Detail)
			}
		}
	}
}

func TestReadMeasurementsRejectsUnexpectedStaleSummary(t *testing.T) {
	commit := strings.Repeat("a", 40)
	tree := strings.Repeat("b", 40)
	directory := t.TempDir()
	for iteration := 1; iteration <= 2; iteration++ {
		data := marshalRunSummary(t, validRunSummary(commit, tree, testRunID, iteration))
		if err := os.WriteFile(
			filepath.Join(directory, "run-"+string(rune('0'+iteration))+".json"),
			data,
			0o600,
		); err != nil {
			t.Fatalf("write run summary %d: %v", iteration, err)
		}
	}

	if _, err := readMeasurements(directory, 1, commit, tree, testRunID); err == nil {
		t.Fatal("unexpected stale run summary was accepted")
	}
}

func validRunSummary(commit, tree, runID string, iteration int) map[string]any {
	outcomes := make([]map[string]any, 0, len(testOutcomeNames))
	for _, name := range testOutcomeNames {
		outcomes = append(outcomes, map[string]any{
			"detail": "observed",
			"name":   name,
			"passed": true,
		})
	}
	return map[string]any{
		"abort_to_upstream_close_ms": 4.5,
		"build_contract": map[string]any{
			"next_legacy_typescript_api_bypassed": true,
			"next_build_id":                       "next-build-123",
			"next_version":                        "16.2.10",
			"react_dom_version":                   "19.2.7",
			"react_version":                       "19.2.7",
			"schema_version":                      2,
			"server_only_version":                 "0.0.1",
			"source_fingerprint":                  strings.Repeat("c", 64),
			"typescript_negative_probe_rejected":  true,
			"typescript_project_typecheck_passed": true,
			"typescript_version":                  "7.0.2",
		},
		"capture_limits": map[string]any{
			"command_output_bytes_per_stream":  1048576,
			"next_server_log_bytes_per_stream": 262144,
		},
		"git_commit":    commit,
		"invocation_id": runID,
		"iteration":     iteration,
		"outcomes":      outcomes,
		"process_result": map[string]any{
			"completed": true,
			"exit_code": 0,
		},
		"production_server": "next start",
		"scan_targets": map[string]any{
			"build_output":        2,
			"downstream_response": 4,
			"next_server_log":     2,
			"server_bundle":       99,
			"source_file":         4,
			"source_map":          31,
			"static_chunk":        15,
		},
		"schema_version":         2,
		"source_tree":            tree,
		"time_to_first_frame_ms": 100.25,
	}
}

func marshalRunSummary(t *testing.T, value map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal run summary: %v", err)
	}
	return data
}
