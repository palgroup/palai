package stack

import (
	"context"
	"fmt"
	"syscall"

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
	if frac < diskFreeFractionFloor {
		return fail(fmt.Sprintf("%s — under the %.0f%% floor (PalaiDiskLow)", detail, diskFreeFractionFloor*100))
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
