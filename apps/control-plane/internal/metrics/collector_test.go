package metrics

import (
	"regexp"
	"strings"
	"testing"
)

// a snapshot deliberately seeded with tenant-shaped values in the PLACES a leak would show up:
// if the renderer ever grew an org/project label, or echoed a tenant id, this fixture would surface
// it. render must expose only lifecycle-enum and loop-name labels.
func leakBaitSnapshot() snapshot {
	return snapshot{
		dbUp:              true,
		runsByState:       map[string]int64{"running": 3, "completed": 7, "failed": 1},
		jobsByStatus:      map[string]int64{"queued": 2, "running": 1, "dead": 4},
		queueReadyDepth:   2,
		queueOldestSec:    12.5,
		inflightOldestSec: 3,
		webhookByState:    map[string]int64{"pending": 5, "dead": 2},
		clockSkewSec:      -0.25,
		runnerSessions:    4,
		restarts:          map[string]int{"dispatch": 0, "reconciler": 1},
		diskFreeBytes:     100,
		diskTotalBytes:    1000,
		objectStore:       objectStoreUp,
		providerCalls:     42,
		providerErrors:    3,
	}
}

func TestRenderIsValidExposition(t *testing.T) {
	var b strings.Builder
	render(&b, leakBaitSnapshot())
	out := b.String()

	// Every metric family must carry a HELP and TYPE line, and a HELP/TYPE must not be orphaned.
	for _, want := range []string{
		"# TYPE palai_db_up gauge",
		"palai_db_up 1",
		`palai_runs{state="completed"} 7`,
		`palai_runs{state="running"} 3`,
		`palai_durable_jobs{status="dead"} 4`,
		"palai_queue_ready_depth 2",
		"palai_queue_oldest_ready_seconds 12.5",
		"palai_job_inflight_oldest_seconds 3",
		`palai_webhook_deliveries{state="dead"} 2`,
		"palai_db_clock_skew_seconds -0.25",
		"palai_runner_sessions 4",
		"# TYPE palai_supervisor_restarts_total counter",
		`palai_supervisor_restarts_total{loop="reconciler"} 1`,
		"palai_disk_free_bytes 100",
		"palai_disk_total_bytes 1000",
		"palai_object_store_up 1",
		"palai_provider_calls_total 42",
		"palai_provider_errors_total 3",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition missing %q\n---\n%s", want, out)
		}
	}

	// Structural: each sample line is `name` or `name{labels}` followed by a value — no trailing
	// junk, no bare label blocks. This is the shape promtool/Prometheus parse.
	sample := regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*(\{[^}]*\})? -?[0-9].*$`)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !sample.MatchString(line) {
			t.Errorf("malformed exposition line: %q", line)
		}
	}
}

// TestRenderLeaksNoTenantIdentity is the load-bearing guarantee: /metrics is unauthenticated, so its
// label set must never carry an organization/project/tenant/secret dimension. Only two label KEYS are
// permitted across the whole surface — `state`/`status` (lifecycle enums) and `loop` (a static
// background-loop name).
func TestRenderLeaksNoTenantIdentity(t *testing.T) {
	var b strings.Builder
	render(&b, leakBaitSnapshot())
	out := b.String()

	labelKey := regexp.MustCompile(`\{([a-zA-Z_][a-zA-Z0-9_]*)=`)
	allowed := map[string]bool{"state": true, "status": true, "loop": true}
	for _, m := range labelKey.FindAllStringSubmatch(out, -1) {
		if !allowed[m[1]] {
			t.Errorf("disallowed metric label %q — /metrics must expose no tenant-identifying dimension", m[1])
		}
	}

	for _, forbidden := range []string{"organization", "org_id", "project", "tenant", "secret", "api_key"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("exposition contains tenant-identifying token %q:\n%s", forbidden, out)
		}
	}
}

// A store that pings nothing means "no object store configured" — the up series must be absent, not
// reported down (a false object-store-down alert on a stack that has no object store).
func TestRenderOmitsObjectStoreWhenAbsent(t *testing.T) {
	s := leakBaitSnapshot()
	s.objectStore = objectStoreAbsent
	s.diskTotalBytes = 0 // also prove the disk series is omitted when statfs failed
	var b strings.Builder
	render(&b, s)
	out := b.String()
	if strings.Contains(out, "palai_object_store_up") {
		t.Errorf("object_store_up must be omitted when no store is configured:\n%s", out)
	}
	if strings.Contains(out, "palai_disk_free_bytes") {
		t.Errorf("disk series must be omitted when statfs was unavailable:\n%s", out)
	}
}
