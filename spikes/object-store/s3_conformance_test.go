package objectstore

import (
	"context"
	"os"
	"testing"
	"time"
)

var expectedConformanceCases = []string{
	"auth.wrong_secret_rejected",
	"bucket.create_and_head",
	"checksum.put_head_get",
	"conditional.if_none_match",
	"range.exact_bytes",
	"multipart.complete",
	"multipart.abort",
	"object.delete_not_found",
	"persistence.seeded",
}

func TestS3SignedReadiness(t *testing.T) {
	configuration, injected := LiveConfigurationFromEnvironment()
	if !injected {
		t.Skip("object-store endpoint/config not injected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ProbeSignedReadiness(ctx, configuration); err != nil {
		t.Fatal(PublicError(err, configuration))
	}
}

func TestS3Conformance(t *testing.T) {
	configuration, injected := LiveConfigurationFromEnvironment()
	if !injected {
		t.Skip("object-store endpoint/config not injected")
	}
	if os.Getenv("PALAI_SPIKE_OBJECT_STORE_PHASE") != "conformance" {
		t.Skip("conformance phase not requested")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	result, err := RunS3Conformance(ctx, configuration)
	if err != nil {
		t.Fatal(PublicError(err, configuration))
	}
	assertObservedCases(t, result.Cases, expectedConformanceCases)
	if err := WriteRunSummary(configuration.SummaryPath, result); err != nil {
		t.Fatal(PublicError(err, configuration))
	}
}

func TestS3RestartPersistence(t *testing.T) {
	configuration, injected := LiveConfigurationFromEnvironment()
	if !injected {
		t.Skip("object-store endpoint/config not injected")
	}
	if os.Getenv("PALAI_SPIKE_OBJECT_STORE_PHASE") != "persistence" {
		t.Skip("persistence phase not requested")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	result, err := VerifyRestartPersistence(ctx, configuration)
	if err != nil {
		t.Fatal(PublicError(err, configuration))
	}
	assertObservedCases(t, result.Cases, []string{
		"persistence.retained_bytes_checksum",
		"persistence.cleanup",
	})
	if err := WriteRunSummary(configuration.SummaryPath, result); err != nil {
		t.Fatal(PublicError(err, configuration))
	}
}

func TestLiveConfigurationRequiresCompleteExplicitInjection(t *testing.T) {
	keys := []string{
		"PALAI_SPIKE_OBJECT_STORE_ENDPOINT",
		"PALAI_SPIKE_OBJECT_STORE_ACCESS_KEY",
		"PALAI_SPIKE_OBJECT_STORE_SECRET_KEY",
		"PALAI_SPIKE_OBJECT_STORE_RUN_ID",
		"PALAI_SPIKE_OBJECT_STORE_ITERATION",
		"PALAI_SPIKE_OBJECT_STORE_SUMMARY",
		"PALAI_SPIKE_GIT_COMMIT",
		"PALAI_SPIKE_SOURCE_TREE",
	}
	for _, key := range keys {
		t.Setenv(key, "")
	}
	if _, injected := LiveConfigurationFromEnvironment(); injected {
		t.Fatal("empty environment was treated as injected live configuration")
	}

	for _, key := range keys {
		t.Setenv(key, "injected")
	}
	t.Setenv("PALAI_SPIKE_OBJECT_STORE_ITERATION", "1")
	if _, injected := LiveConfigurationFromEnvironment(); !injected {
		t.Fatal("complete explicit environment was not recognized")
	}

	t.Setenv("PALAI_SPIKE_OBJECT_STORE_SECRET_KEY", "")
	if _, injected := LiveConfigurationFromEnvironment(); injected {
		t.Fatal("partial environment was treated as injected live configuration")
	}
}

func assertObservedCases(t *testing.T, observed map[string]time.Duration, expected []string) {
	t.Helper()
	if len(observed) != len(expected) {
		t.Fatalf("observed case count = %d, want %d", len(observed), len(expected))
	}
	for _, name := range expected {
		if latency, ok := observed[name]; !ok || latency < 0 {
			t.Fatalf("case %q was not observed", name)
		}
	}
}
