package stack

import (
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/storage"
)

// The queue and callback checks reuse the named statements in storage/queries/metrics.sql rather
// than forking the SQL. storage.Query PANICS on an unknown name, so resolving both names here
// guarantees checkQueue/checkCallback will not blow up at runtime on a typo. Their result SHAPES
// (int64,float64) and (string,int64) are already proven live by the shipped T6 collector, which
// scans the identical statements into the identical Go types.
func TestDoctorReusesMetricsQueries(t *testing.T) {
	for _, name := range []string{"MetricQueueReady", "MetricWebhookDeliveryStates"} {
		if q := storage.Query(name); strings.TrimSpace(q) == "" {
			t.Fatalf("storage.Query(%q) is empty", name)
		}
	}
}

// The three E14 T3 doctor-v2 checks are pure threshold functions over already-fetched
// signals (statfs bytes, queue depth/age, webhook delivery states), so their green/fail
// boundaries are proven with no database and no Docker. The thresholds mirror the §52.10
// alerts in deploy/observability/alerts.yml — doctor must fire exactly when the alert would.

func TestDiskCheck(t *testing.T) {
	// 20% free is above the 10% floor → green.
	if c := diskCheck(20, 100); c.Status != "ok" {
		t.Fatalf("20%% free should be ok, got %q (%s)", c.Status, c.Detail)
	}
	// Exactly at the floor (10%) is NOT under it → green (alert is strict `< 0.10`).
	if c := diskCheck(10, 100); c.Status != "ok" {
		t.Fatalf("10%% free (at floor) should be ok, got %q (%s)", c.Status, c.Detail)
	}
	// Below the floor → fail.
	if c := diskCheck(9, 100); c.Status != "fail" {
		t.Fatalf("9%% free should fail, got %q (%s)", c.Status, c.Detail)
	}
	// A statfs that reports zero total is a broken read, not a healthy empty disk.
	if c := diskCheck(0, 0); c.Status != "fail" {
		t.Fatalf("zero total bytes should fail, got %q (%s)", c.Status, c.Detail)
	}
}

func TestQueueCheck(t *testing.T) {
	// Empty queue → green.
	if c := queueCheck(0, 0); c.Status != "ok" {
		t.Fatalf("empty queue should be ok, got %q (%s)", c.Status, c.Detail)
	}
	// A deep-but-fresh backlog is green: depth alone never fails, only a stalled age does.
	if c := queueCheck(9999, 42); c.Status != "ok" {
		t.Fatalf("deep-but-fresh queue should be ok, got %q (%s)", c.Status, c.Detail)
	}
	// At the 300s boundary is not over it → green.
	if c := queueCheck(1, 300); c.Status != "ok" {
		t.Fatalf("300s oldest (at boundary) should be ok, got %q (%s)", c.Status, c.Detail)
	}
	// Past 300s → fail (dispatch not keeping up).
	if c := queueCheck(1, 301); c.Status != "fail" {
		t.Fatalf("301s oldest should fail, got %q (%s)", c.Status, c.Detail)
	}
}

func TestCallbackCheck(t *testing.T) {
	// No deliveries → green.
	if c := callbackCheck(map[string]int64{}); c.Status != "ok" {
		t.Fatalf("no deliveries should be ok, got %q (%s)", c.Status, c.Detail)
	}
	// Dead-letters are surfaced but do not fail a point-in-time check (the alert is a delta).
	dead := callbackCheck(map[string]int64{"dead": 3})
	if dead.Status != "ok" {
		t.Fatalf("dead-letters should stay ok-but-named, got %q", dead.Status)
	}
	if !strings.Contains(dead.Detail, "3 dead") {
		t.Fatalf("dead count must be named in detail, got %q", dead.Detail)
	}
	// A pending backlog over 50 fails (delivery pump not draining).
	if c := callbackCheck(map[string]int64{"pending": 51}); c.Status != "fail" {
		t.Fatalf("51 pending should fail, got %q (%s)", c.Status, c.Detail)
	}
	// 50 pending is at the boundary, not over → green.
	if c := callbackCheck(map[string]int64{"pending": 50}); c.Status != "ok" {
		t.Fatalf("50 pending (at boundary) should be ok, got %q (%s)", c.Status, c.Detail)
	}
}

// TestRunnerIdentityCheck pins the fifteenth check's boundaries, which are the difference
// between "the runner is fine" and the deadlock that made every run fail with nothing pointing
// at the cause. Only one combination is a fault — expired AND no session — and the failing
// detail must NAME expiry, because that is the whole reason the check exists.
func TestRunnerIdentityCheck(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	const dns = "runner-local.runners.palai.internal"

	// Nothing has enrolled yet: a stack in its first seconds is not broken.
	if c := runnerIdentityCheck("", time.Time{}, now, 0); c.Status != "ok" {
		t.Fatalf("no identity yet should be ok, got %q (%s)", c.Status, c.Detail)
	}
	// A valid identity is green and says how long is left.
	live := runnerIdentityCheck(dns, now.Add(90*time.Second), now, 1)
	if live.Status != "ok" {
		t.Fatalf("a valid identity should be ok, got %q (%s)", live.Status, live.Detail)
	}
	if !strings.Contains(live.Detail, "1m30s") {
		t.Fatalf("remaining lifetime must be named, got %q", live.Detail)
	}
	// Expired but a session is still parked: a parked WebSocket outlives the certificate that
	// opened it and the runner is mid-recovery — named, not failed.
	recovering := runnerIdentityCheck(dns, now.Add(-30*time.Second), now, 1)
	if recovering.Status != "ok" {
		t.Fatalf("expired-with-a-session should stay ok-but-named, got %q (%s)", recovering.Status, recovering.Detail)
	}
	if !strings.Contains(recovering.Detail, "expired") {
		t.Fatalf("expiry must be named even when it is not a fault, got %q", recovering.Detail)
	}
	// The deadlock: expired and nothing connected.
	dead := runnerIdentityCheck(dns, now.Add(-9*time.Minute), now, 0)
	if dead.Status != "fail" {
		t.Fatalf("expired with no session is the deadlock and must fail, got %q (%s)", dead.Status, dead.Detail)
	}
	for _, want := range []string{"EXPIRED", "9m0s", dns, "2026-07-27T11:51:00Z", "palai local down && palai local up"} {
		if !strings.Contains(dead.Detail, want) {
			t.Fatalf("the failing detail must name %q so the operator is not left guessing, got %q", want, dead.Detail)
		}
	}
}
