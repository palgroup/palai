// Package metrics exposes the control plane's operational signals in Prometheus text-exposition
// format on an unauthenticated, internal-network GET /metrics (mounted beside /healthz — the
// production TLS edge proxies /v1 only, never this). Every series is INSTALLATION-AGGREGATE:
// grouped by lifecycle enum or background-loop name, NEVER by organization/project/secret, so an
// unauthenticated scrape leaks no tenant identity (asserted in collector_test.go). The bundle in
// deploy/observability/ (§52.9 dashboards, §52.10 alerts) is the sole consumer.
//
// ponytail: a hand-written text writer for ~15 gauges/counters — no Prometheus client dependency.
// The plan's ladder: reach for a client library only when a histogram/quantile is actually needed;
// today every series is a scalar the durable spine already knows, so a few Fprintf lines beat a
// registry. Add the dependency the day a latency histogram earns its keep.
package metrics

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/storage"
)

// Provider-call counters are process-wide because the two call sites (the run dispatcher and the
// MCP-sampling gate) live in a package the shared model-broker cannot import back into. This is the
// standard expvar/Prometheus global-registry shape: RecordProviderCall is one line at each call
// site, no metrics sink threaded through the broker's constructor. They are cumulative for the
// process lifetime (reset on restart, like any Prometheus _total), which is exactly what a
// rate()/increase() alert expects.
var (
	providerCalls  atomic.Int64
	providerErrors atomic.Int64
)

// RecordProviderCall counts one provider model call, and one error when err != nil. Call it at every
// site that dials a provider through the model broker so palai_provider_errors_total reflects the
// real upstream, not a stub.
func RecordProviderCall(err error) {
	providerCalls.Add(1)
	if err != nil {
		providerErrors.Add(1)
	}
}

// ObjectStorePinger is a read-only reachability probe (a HEAD, not a write) the collector calls once
// per scrape to publish palai_object_store_up. nil when the stack runs with no object store, and the
// series is then omitted rather than reported down.
type ObjectStorePinger interface {
	Ping(ctx context.Context) error
}

// Collector gathers the operational snapshot and renders it. Every optional dependency is nil-safe:
// a stack with no runner gateway reports zero sessions, one with no object store omits the up series.
type Collector struct {
	pool     *pgxpool.Pool
	runners  func() int64          // gateway connected-session count; nil => 0 (assignment-only tiers)
	restarts func() map[string]int // supervisor per-loop restart counters; nil => none
	objStore ObjectStorePinger     // nil => palai_object_store_up omitted
	diskPath string                // filesystem to statfs for palai_disk_*_bytes
	now      func() time.Time
}

// New wires the collector. diskPath is the filesystem whose free/total bytes back the disk alert
// (the operator points it at the data volume mount; defaults to "/" when empty).
func New(pool *pgxpool.Pool, runners func() int64, restarts func() map[string]int, objStore ObjectStorePinger, diskPath string) *Collector {
	if diskPath == "" {
		diskPath = "/"
	}
	return &Collector{pool: pool, runners: runners, restarts: restarts, objStore: objStore, diskPath: diskPath, now: time.Now}
}

// ServeHTTP gathers a fresh snapshot and writes it as Prometheus text exposition. A partial DB
// failure still returns 200 with palai_db_up 0 (so Prometheus keeps target `up` at 1 and the
// PalaiScrapeDBDown alert fires) rather than a 500 that would look like the process is gone.
func (c *Collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A slow database must not hang the scrape; bound the whole gather.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	snap := c.gather(ctx)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	bw := bufio.NewWriter(w)
	render(bw, snap)
	_ = bw.Flush()
}

// snapshot is the point-in-time metric set. Splitting the gather (DB + host reads) from render (the
// text formatter) makes the exposition format and the no-tenant-leak guarantee unit-testable with no
// database (collector_test.go feeds this struct directly).
type snapshot struct {
	dbUp              bool
	runsByState       map[string]int64
	jobsByStatus      map[string]int64
	queueReadyDepth   int64
	queueOldestSec    float64
	inflightOldestSec float64
	webhookByState    map[string]int64
	clockSkewSec      float64
	runnerSessions    int64
	restarts          map[string]int
	diskFreeBytes     uint64
	diskTotalBytes    uint64
	objectStore       objectStoreState
	providerCalls     int64
	providerErrors    int64
}

type objectStoreState int

const (
	objectStoreAbsent objectStoreState = iota // no store configured — series omitted
	objectStoreUp
	objectStoreDown
)

func (c *Collector) gather(ctx context.Context) snapshot {
	s := snapshot{
		runsByState:    map[string]int64{},
		jobsByStatus:   map[string]int64{},
		webhookByState: map[string]int64{},
		providerCalls:  providerCalls.Load(),
		providerErrors: providerErrors.Load(),
	}
	if c.runners != nil {
		s.runnerSessions = c.runners()
	}
	if c.restarts != nil {
		s.restarts = c.restarts()
	}

	// The metrics scan is cross-tenant BY CONSTRUCTION — an installation-wide count cannot be scoped
	// to one tenant. This is the documented WithSystemScope escape hatch (storage/tenant.go); it is
	// safe here precisely because every query groups by a lifecycle enum, never by tenant, so the
	// aggregate exposes no organization/project row.
	sctx := storage.WithSystemScope(ctx)
	s.dbUp = c.gatherDB(sctx, &s)

	if c.objStore != nil {
		if err := c.objStore.Ping(ctx); err != nil {
			s.objectStore = objectStoreDown
		} else {
			s.objectStore = objectStoreUp
		}
	}

	var st syscall.Statfs_t
	if err := syscall.Statfs(c.diskPath, &st); err == nil {
		// Bsize is the fundamental block size; Bavail is blocks free to a non-root writer (the honest
		// "space you can actually use"), Blocks the total.
		// ponytail: measures the control-plane container's view of diskPath — the operator mounts the
		// data volume there (PALAI_METRICS_DISK_PATH). A true multi-volume host needs one series per
		// mount; single-node alpha has one data volume.
		s.diskFreeBytes = uint64(st.Bavail) * uint64(st.Bsize)
		s.diskTotalBytes = uint64(st.Blocks) * uint64(st.Bsize)
	}
	return s
}

// gatherDB runs the aggregate queries. It returns false on the first failure so palai_db_up reports
// the outage; whatever was read before the failure is still rendered.
func (c *Collector) gatherDB(ctx context.Context, s *snapshot) bool {
	if c.pool == nil {
		return false
	}
	if err := scanCounts(ctx, c.pool, storage.Query("MetricRunStates"), s.runsByState); err != nil {
		return false
	}
	if err := scanCounts(ctx, c.pool, storage.Query("MetricJobStatuses"), s.jobsByStatus); err != nil {
		return false
	}
	if err := c.pool.QueryRow(ctx, storage.Query("MetricQueueReady")).Scan(&s.queueReadyDepth, &s.queueOldestSec); err != nil {
		return false
	}
	if err := c.pool.QueryRow(ctx, storage.Query("MetricJobInflightOldest")).Scan(&s.inflightOldestSec); err != nil {
		return false
	}
	if err := scanCounts(ctx, c.pool, storage.Query("MetricWebhookDeliveryStates"), s.webhookByState); err != nil {
		return false
	}
	var dbNow time.Time
	before := c.now()
	if err := c.pool.QueryRow(ctx, storage.Query("MetricDBClock")).Scan(&dbNow); err != nil {
		return false
	}
	s.clockSkewSec = dbNow.Sub(before).Seconds()
	return true
}

// scanCounts loads a two-column "label, count" aggregate into m.
func scanCounts(ctx context.Context, pool *pgxpool.Pool, query string, m map[string]int64) error {
	rows, err := pool.Query(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var label string
		var n int64
		if err := rows.Scan(&label, &n); err != nil {
			return err
		}
		m[label] = n
	}
	return rows.Err()
}

// render writes the exposition. It is pure (no DB, no clock) so the format and the tenant-safety of
// the label set are testable in isolation.
func render(w io.Writer, s snapshot) {
	up := 0.0
	if s.dbUp {
		up = 1
	}
	writeScalar(w, "palai_db_up", "gauge", "1 when the aggregate metric queries succeeded this scrape, 0 on a database read failure.", up)

	writeLabeled(w, "palai_runs", "gauge", "Runs by lifecycle state (installation-aggregate).", "state", s.runsByState)
	writeLabeled(w, "palai_durable_jobs", "gauge", "Durable jobs by status (installation-aggregate).", "status", s.jobsByStatus)
	writeScalar(w, "palai_queue_ready_depth", "gauge", "Queued durable jobs whose ready_at has arrived (claimable backlog).", float64(s.queueReadyDepth))
	writeScalar(w, "palai_queue_oldest_ready_seconds", "gauge", "Age of the oldest claimable queued job; the queue-backlog alert reads this.", s.queueOldestSec)
	writeScalar(w, "palai_job_inflight_oldest_seconds", "gauge", "Age of the oldest running job — a dispatch-progress proxy (not a full latency histogram).", s.inflightOldestSec)
	writeLabeled(w, "palai_webhook_deliveries", "gauge", "Outbound webhook (callback) deliveries by state; pending is backlog, dead is failure.", "state", s.webhookByState)
	writeScalar(w, "palai_db_clock_skew_seconds", "gauge", "Database clock minus control-plane clock; the clock-skew alert reads its magnitude.", s.clockSkewSec)

	writeScalar(w, "palai_runner_sessions", "gauge", "Runner sessions currently connected to the gateway; 0 means no runner (the runner-down alert reads this).", float64(s.runnerSessions))

	writeHeader(w, "palai_supervisor_restarts_total", "counter", "Background-loop restarts since boot, by loop name.")
	for _, loop := range sortedKeys(s.restarts) {
		fmt.Fprintf(w, "palai_supervisor_restarts_total{loop=%q} %d\n", loop, s.restarts[loop])
	}

	if s.diskTotalBytes > 0 {
		writeScalar(w, "palai_disk_free_bytes", "gauge", "Free bytes on the data filesystem (the disk-low alert reads free/total).", float64(s.diskFreeBytes))
		writeScalar(w, "palai_disk_total_bytes", "gauge", "Total bytes on the data filesystem.", float64(s.diskTotalBytes))
	}

	if s.objectStore != objectStoreAbsent {
		v := 0.0
		if s.objectStore == objectStoreUp {
			v = 1
		}
		writeScalar(w, "palai_object_store_up", "gauge", "1 when the object store answered a HEAD this scrape, 0 when it did not (the object-store alert reads this).", v)
	}

	writeScalar(w, "palai_provider_calls_total", "counter", "Provider model calls attempted since boot.", float64(s.providerCalls))
	writeScalar(w, "palai_provider_errors_total", "counter", "Provider model calls that returned an error since boot (the provider-error alert reads its rate).", float64(s.providerErrors))
}

func writeHeader(w io.Writer, name, typ, help string) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func writeScalar(w io.Writer, name, typ, help string, v float64) {
	writeHeader(w, name, typ, help)
	fmt.Fprintf(w, "%s %s\n", name, formatFloat(v))
}

// writeLabeled always emits HELP/TYPE (so a scrape sees the series even at zero rows) and one line
// per label value, sorted for deterministic output. Label values are CHECK-constrained DB lifecycle
// enums (never free-form tenant input), so %q's double-quote/backslash/newline escaping is exactly
// the Prometheus label-value escaping they need.
func writeLabeled(w io.Writer, name, typ, help, label string, m map[string]int64) {
	writeHeader(w, name, typ, help)
	for _, k := range sortedKeys(m) {
		fmt.Fprintf(w, "%s{%s=%q} %d\n", name, label, k, m[k])
	}
}

func formatFloat(v float64) string {
	// Whole numbers render without a decimal point (counts, bytes); fractional values keep precision.
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
