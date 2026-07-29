package stack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/storage"
)

// doctor_v2.go adds the three E14 T3 operability checks ON TOP of the eleven in doctor.go
// (disk, queue, callback). Each reads the SAME real signal E14 T6's /metrics exposes — the
// disk statfs the collector runs and the two aggregate queries in storage/queries/metrics.sql
// — and each fails on the SAME boundary as its §52.10 alert in deploy/observability/alerts.yml,
// so an operator's on-demand `doctor` verdict agrees with what Prometheus would fire. The
// pure threshold functions (diskCheck/queueCheck/callbackCheck) hold the boundaries and are
// unit-tested with no database (doctor_v2_test.go); the wrappers only fetch the signal.

const (
	// diskFreeFractionFloor mirrors PalaiDiskLow (free/total < 0.10).
	diskFreeFractionFloor = 0.10
	// diskFreeAbsoluteFloorBytes mirrors PalaiDiskFloor (palai_disk_free_bytes < 5GiB).
	//
	// A ratio ALONE gives the smallest machines the weakest floor: on a 20 GiB cloud VM — a normal
	// size for this single-node stack — 10% is 2 GiB and reads green. Derived from what the stack
	// actually consumes, measured 2026-07-29 (docs/operations/install.md "Resource minimums"):
	//
	//   0.89 GiB  the six images on disk (postgres 451.7 + seaweedfs 229.6 + engine 137.5 +
	//             caddy 47.0 + control-plane 24.7 + runner 14.7 MiB)
	//   0.18 GiB  an upgrade's headroom: `palai upgrade` pulls control-plane+runner+engine N+1
	//             BEFORE the N images are removed, so both generations are resident at once
	//   1x data   a backup writes its whole archive to disk beside the Postgres + object-store
	//             data it just dumped, and dockerCapture buffers the dump in memory too
	//   the rest  Postgres WAL, per-run engine containers and their workspaces, and container
	//             logs, which compose leaves unrotated by default
	//
	// 5 GiB is the round number above the two fixed costs (1.07 GiB) that still leaves a small
	// install room to take a backup of itself. It is a FLOOR, not a sizing recommendation — the
	// documented minimum disk is 20 GiB.
	diskFreeAbsoluteFloorBytes = uint64(5) << 30
	// queueOldestReadyMaxSec mirrors PalaiQueueBacklog (palai_queue_oldest_ready_seconds > 300).
	queueOldestReadyMaxSec = 300.0
	// webhookPendingMax mirrors PalaiWebhookDeliveryBacklog (pending > 50).
	webhookPendingMax = 50
)

// checkDisk reports free space on the stack's data dir.
//
// ponytail: it statfs's the HOST data dir (.palai — certs, secrets, config), while /metrics
// statfs's the control-plane container's mounted data VOLUME. On a single-node host both sit on
// the same physical disk, so this is the same disk-low signal reachable without the container.
// A true multi-volume host would want one reading per mount; single-node alpha has one data dir.
func checkDisk(cfg Config) Check {
	var st syscall.Statfs_t
	if err := syscall.Statfs(cfg.DataDir, &st); err != nil {
		return fail(fmt.Sprintf("statfs data dir %s: %v", cfg.DataDir, err))
	}
	// Bavail is blocks free to a non-root writer (the honest usable free), Blocks the total;
	// mirror the collector's casts so darwin (test host) and linux (container) both build.
	return diskCheck(uint64(st.Bavail)*uint64(st.Bsize), uint64(st.Blocks)*uint64(st.Bsize))
}

func diskCheck(freeBytes, totalBytes uint64) Check {
	if totalBytes == 0 {
		return fail("statfs reported zero total bytes for the data dir")
	}
	frac := float64(freeBytes) / float64(totalBytes)
	detail := fmt.Sprintf("data dir %.1f%% free (%s of %s)", frac*100, humanBytes(freeBytes), humanBytes(totalBytes))
	// Two floors, and the MORE demanding one wins: a big disk is governed by the ratio (10% of
	// 460GiB is 46GiB), a small one by the absolute floor (10% of 20GiB is 2GiB, which the stack
	// cannot live on). Report the ratio first when both are breached — it is the older alert and
	// the bigger number.
	if frac < diskFreeFractionFloor {
		return fail(fmt.Sprintf("%s — under the %.0f%% floor (PalaiDiskLow)", detail, diskFreeFractionFloor*100))
	}
	if freeBytes < diskFreeAbsoluteFloorBytes {
		return fail(fmt.Sprintf("%s — under the %s absolute floor (PalaiDiskFloor); the stack's own images are ~0.9GiB and a backup writes a full copy of its data beside it", detail, humanBytes(diskFreeAbsoluteFloorBytes)))
	}
	return ok(detail)
}

// checkQueue reports the claimable-backlog depth and the age of its oldest member, reusing the
// MetricQueueReady query /metrics reads. Doctor connects as the Postgres superuser, so the
// installation-wide count is not narrowed by row-level security (unlike the collector, which
// takes the WithSystemScope path under the non-owner role).
func checkQueue(ctx context.Context, pgURL string) Check {
	conn, err := pgx.Connect(ctx, pgURL)
	if err != nil {
		return fail("connect Postgres: " + err.Error())
	}
	defer conn.Close(ctx)
	var depth int64
	var oldestReadySec float64
	if err := conn.QueryRow(ctx, storage.Query("MetricQueueReady")).Scan(&depth, &oldestReadySec); err != nil {
		return fail("read queue depth: " + err.Error())
	}
	return queueCheck(depth, oldestReadySec)
}

func queueCheck(readyDepth int64, oldestReadySec float64) Check {
	detail := fmt.Sprintf("%d claimable queued, oldest ready %.0fs", readyDepth, oldestReadySec)
	if oldestReadySec > queueOldestReadyMaxSec {
		return fail(fmt.Sprintf("%s — over %.0fs, dispatch not keeping up (PalaiQueueBacklog)", detail, queueOldestReadyMaxSec))
	}
	return ok(detail)
}

// checkCallback reports outbound-webhook (callback) delivery health, reusing the
// MetricWebhookDeliveryStates query /metrics reads.
func checkCallback(ctx context.Context, pgURL string) Check {
	conn, err := pgx.Connect(ctx, pgURL)
	if err != nil {
		return fail("connect Postgres: " + err.Error())
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, storage.Query("MetricWebhookDeliveryStates"))
	if err != nil {
		return fail("read webhook deliveries: " + err.Error())
	}
	defer rows.Close()
	states := map[string]int64{}
	for rows.Next() {
		var state string
		var n int64
		if err := rows.Scan(&state, &n); err != nil {
			return fail("scan webhook deliveries: " + err.Error())
		}
		states[state] = n
	}
	if err := rows.Err(); err != nil {
		return fail("iterate webhook deliveries: " + err.Error())
	}
	return callbackCheck(states)
}

// callbackCheck fails on a pending backlog the delivery pump is not draining (PalaiWebhookDeliveryBacklog).
// Dead-letters are surfaced in the detail but do not fail: the alert for them (PalaiWebhookDeadLetters)
// is a delta over a window, which a point-in-time check cannot compute — a lingering dead row from last
// week is history, not a current outage, so doctor names it green (the checkSupervisor idiom).
func callbackCheck(states map[string]int64) Check {
	pending := states["pending"]
	dead := states["dead"]
	detail := fmt.Sprintf("%d pending, %d dead-lettered", pending, dead)
	if pending > webhookPendingMax {
		return fail(fmt.Sprintf("%s — pending over %d, delivery pump not draining (PalaiWebhookDeliveryBacklog)", detail, webhookPendingMax))
	}
	return ok(detail)
}

// checkRunnerIdentity names the one failure the rest of doctor cannot see: the runner's client
// certificate lapsed. Every other check is green while this happens — the gateway listens, its
// mTLS rejection works, the schema is current — and the only symptom is that runs fail, because
// the runner can neither renew (renewal authenticates with the certificate that expired) nor
// connect. The signal is /healthz/runner: the control plane records the certificate a runner
// last presented, and a healthy runner refreshes that record on every renewal.
func checkRunnerIdentity(ctx context.Context, cfg Config) Check {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+"/healthz/runner", nil)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return fail("GET /healthz/runner: " + err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fail(fmt.Sprintf("GET /healthz/runner = %d, want 200", resp.StatusCode))
	}
	var body struct {
		Gateway  bool  `json:"gateway"`
		Sessions int64 `json:"sessions"`
		Identity *struct {
			RunnerDNS string    `json:"runner_dns"`
			NotAfter  time.Time `json:"not_after"`
		} `json:"identity"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fail("decode /healthz/runner: " + err.Error())
	}
	if !body.Gateway {
		return ok("no runner gateway configured on this control plane")
	}
	if body.Identity == nil {
		return runnerIdentityCheck("", time.Time{}, time.Time{}, body.Sessions)
	}
	return runnerIdentityCheck(body.Identity.RunnerDNS, body.Identity.NotAfter, time.Now(), body.Sessions)
}

// runnerIdentityCheck is the verdict, kept pure so its boundaries are unit-tested without a
// stack. It fails on exactly one condition — the last identity the control plane saw has expired
// AND no runner session is connected — because that combination is the deadlock and nothing else
// is. An expired record with a session still parked is normal: a parked WebSocket outlives the
// certificate that opened it, and the runner is mid-recovery. A zero NotAfter means no runner has
// presented a certificate yet, which is what a stack looks like for the first seconds after
// `local up`.
func runnerIdentityCheck(runnerDNS string, notAfter, now time.Time, sessions int64) Check {
	if notAfter.IsZero() {
		return ok("no runner has presented an identity yet (the runner enrolls a few seconds after `palai local up`)")
	}
	remaining := notAfter.Sub(now)
	if remaining > 0 {
		return ok(fmt.Sprintf("runner %s identity valid for another %s (until %s), %d session(s) connected",
			runnerDNS, remaining.Round(time.Second), notAfter.UTC().Format(time.RFC3339), sessions))
	}
	if sessions > 0 {
		return ok(fmt.Sprintf("runner %s identity expired %s ago (at %s) but %d session(s) are connected — the runner is rolling it forward",
			runnerDNS, (-remaining).Round(time.Second), notAfter.UTC().Format(time.RFC3339), sessions))
	}
	return fail(fmt.Sprintf("runner %s identity EXPIRED %s ago (at %s) and no runner session is connected — runs will fail until it re-enrolls; the runner recovers itself from PALAI_ENROLLMENT_TOKEN_FILE, so if this persists check `docker logs` for the runner and then run `palai local down && palai local up`",
		runnerDNS, (-remaining).Round(time.Second), notAfter.UTC().Format(time.RFC3339)))
}

// humanBytes renders a byte count in binary units for the operator-facing detail.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
