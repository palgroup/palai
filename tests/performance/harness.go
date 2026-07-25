// Package performance is the E18 Task 6 performance harness (PER-001..004, spec §54.3, §64.14).
//
// WHAT IT IS: the gate MECHANISM, not a benchmark publication. Each PER case drives a real local seam
// (the running control-plane binary, the real E09 workspace/lease spine, the real OCI fixture engine,
// the real E11 trigger admission gate), records RAW samples, and derives percentiles from those raw
// samples. The four rules the mechanism enforces:
//
//  1. NO NUMBER WITHOUT A PROFILE. A run that cannot stamp machine/cores/memory/OS/Docker/load-shape
//     produces NO result at all — Complete writes nothing and returns ErrNoProfile. A number with no
//     profile is not a measurement, it is a rumour.
//  2. RECOMPUTE-OVER-COPY. Raw samples go to samples.jsonl; summary.json's percentiles are DERIVED from
//     them by a documented method (nearest-rank) and the summary carries the samples digest, so E18 T10's
//     verifier re-computes them from the raw file instead of trusting the copy.
//  3. THRESHOLDS ARE CONFIGURABLE. §54.3's targets are the defaults; PALAI_PERF_MAX_<METRIC> overrides
//     any of them and the summary records that the value came from the environment. The gate is the
//     deliverable, the numbers are an input.
//  4. HONEST CEILING, CARRIED IN THE ARTIFACT. Every run stamps its own ceiling text into profile.json.
//
// HONEST CEILING (applies to every number this package produces): these are macOS + Docker-Desktop
// profile numbers from a shared developer laptop. They carry NO SLO and NO reference-hardware claim —
// §54.2/§54.3 targets are PRODUCT GOALS whose real measurement is the E18 §6 operator leg 3. "Soak"
// here is a BOUNDED window of minutes (PER-003); a real long soak is §6 leg 7. Per the E08 rule the
// engine opens no tool to a real provider, so no number here is model behaviour.
package performance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrNoProfile is the refusal every PER case inherits: a run whose hardware/load profile could not be
// stamped MUST NOT produce a result (design invariant §2, "sayı ancak profille"). Complete returns it
// and writes no artifact at all — not even the raw samples, because a sample with no machine behind it
// cannot be compared to anything.
var ErrNoProfile = errors.New("performance: refusing to produce a result without a complete hardware/load profile")

// ErrThreshold reports a gate failure: at least one metric's gated percentile exceeded its threshold, or
// the run's error rate exceeded its ceiling. The artifacts ARE written in this case — a failed
// measurement is still a measurement, and the RED fixtures depend on reading it back.
var ErrThreshold = errors.New("performance: threshold gate failed")

// Profile is the mandatory stamp. Every field except Notes is REQUIRED; validate() rejects a partial
// stamp, which is what makes "no number without a profile" mechanical rather than aspirational.
type Profile struct {
	Case        string    `json:"case"`
	LoadShape   string    `json:"load_shape"`
	Machine     string    `json:"machine"`
	OS          string    `json:"os"`
	Arch        string    `json:"arch"`
	Cores       int       `json:"cores"`
	MemoryBytes int64     `json:"memory_bytes"`
	Docker      string    `json:"docker"`
	Go          string    `json:"go"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	Ceiling     string    `json:"ceiling"`
}

func (p Profile) validate() error {
	missing := []string{}
	check := func(name string, empty bool) {
		if empty {
			missing = append(missing, name)
		}
	}
	check("case", p.Case == "")
	check("load_shape", p.LoadShape == "")
	check("machine", p.Machine == "")
	check("os", p.OS == "")
	check("arch", p.Arch == "")
	check("cores", p.Cores <= 0)
	check("memory_bytes", p.MemoryBytes <= 0)
	check("docker", p.Docker == "")
	check("go", p.Go == "")
	check("ceiling", p.Ceiling == "")
	check("started_at", p.StartedAt.IsZero())
	if len(missing) > 0 {
		return fmt.Errorf("%w (missing: %s)", ErrNoProfile, strings.Join(missing, ", "))
	}
	return nil
}

// Sample is one raw observation. Latencies carry Unit "ns"; gauges carry their own unit (bytes, count).
// Every sample is written verbatim to samples.jsonl — the percentiles in summary.json are derived from
// exactly these lines and nothing else.
type Sample struct {
	Metric string    `json:"metric"`
	Phase  string    `json:"phase,omitempty"`
	Value  float64   `json:"value"`
	Unit   string    `json:"unit"`
	OK     bool      `json:"ok"`
	At     time.Time `json:"at"`
}

// Threshold is one configured gate: the percentile it reads (100 = max) and the ceiling that percentile
// must stay under. Source records whether the number is §54.3's default or an environment override, so a
// loosened run says so in its own evidence.
type Threshold struct {
	Percentile int     `json:"percentile"`
	Max        float64 `json:"max"`
	Unit       string  `json:"unit"`
	Source     string  `json:"source"`
}

// Stat is one metric's derived result. The percentiles are computed from the raw samples by
// percentile() below; T10's verifier re-computes them the same way from samples.jsonl.
type Stat struct {
	Metric    string     `json:"metric"`
	Unit      string     `json:"unit"`
	Count     int        `json:"count"`
	Errors    int        `json:"errors"`
	ErrorRate float64    `json:"error_rate"`
	Min       float64    `json:"min"`
	P50       float64    `json:"p50"`
	P95       float64    `json:"p95"`
	P99       float64    `json:"p99"`
	Max       float64    `json:"max"`
	Gate      *Threshold `json:"gate,omitempty"`
	GateValue float64    `json:"gate_value,omitempty"`
	Pass      bool       `json:"pass"`
}

// Summary is summary.json: the derived view plus the provenance a re-computing verifier needs
// (the samples file, its digest, and the exact percentile method).
type Summary struct {
	Case              string   `json:"case"`
	PercentileMethod  string   `json:"percentile_method"`
	SamplesFile       string   `json:"samples_file"`
	SamplesSHA256     string   `json:"samples_sha256"`
	SampleCount       int      `json:"sample_count"`
	MaxErrorRate      float64  `json:"max_error_rate"`
	Stats             []Stat   `json:"stats"`
	Failures          []string `json:"failures"`
	Pass              bool     `json:"pass"`
	DerivedFromRaw    bool     `json:"derived_from_raw_samples"`
	ProfileFile       string   `json:"profile_file"`
	ProfileSHA256     string   `json:"profile_sha256"`
	NoSLOClaim        bool     `json:"no_slo_claim"`
	ReferenceHardware bool     `json:"reference_hardware"`
}

// percentileMethod is the contract between this harness and T10's verifier. Nearest-rank on the
// ascending sort: index = ceil(p/100 * n) - 1, clamped to [0, n-1]; p100 is therefore the max. It is
// stated in every summary so a re-computation cannot silently use a different interpolation.
const percentileMethod = "nearest-rank: sort ascending, index = ceil(p/100*n)-1 clamped to [0,n-1]"

// Run accumulates one PER case's samples behind a stamped profile.
type Run struct {
	profile      Profile
	samples      []Sample
	gates        map[string]Threshold
	units        map[string]string
	maxErrorRate float64
}

// NewRun stamps the profile and opens a run. It returns ErrNoProfile when the machine cannot be
// described — a caller that ignores the error and measures anyway still cannot Complete.
func NewRun(caseID, loadShape, ceiling string) (*Run, error) {
	p, err := detectProfile()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoProfile, err)
	}
	p.Case, p.LoadShape, p.Ceiling = caseID, loadShape, ceiling
	p.StartedAt = time.Now().UTC()
	r := &Run{profile: p, gates: map[string]Threshold{}, units: map[string]string{}}
	if err := r.profile.validate(); err != nil {
		return nil, err
	}
	return r, nil
}

// Latency records one timed operation. ok=false marks it an error for the error-rate gate; the sample is
// still recorded, because dropping failed attempts is how a load test lies about its error rate.
func (r *Run) Latency(metric, phase string, d time.Duration, ok bool) {
	r.Observe(metric, phase, float64(d.Nanoseconds()), "ns", ok)
}

// Observe records one raw observation in its own unit (a queue depth, an RSS reading, a byte count).
func (r *Run) Observe(metric, phase string, value float64, unit string, ok bool) {
	r.samples = append(r.samples, Sample{Metric: metric, Phase: phase, Value: value, Unit: unit, OK: ok, At: time.Now().UTC()})
	r.units[metric] = unit
}

// Gate configures the threshold for a metric: percentile pct (100 = max) must stay at or under max.
// The default is §54.3's target; PALAI_PERF_MAX_<METRIC> overrides it (a Go duration for ns metrics, a
// bare number otherwise) and the override is recorded in the summary.
func (r *Run) Gate(metric string, pct int, max float64, unit string) {
	g := Threshold{Percentile: pct, Max: max, Unit: unit, Source: "default (spec §54.3 / case budget)"}
	if raw := os.Getenv(thresholdEnv(metric)); raw != "" {
		if v, ok := parseThreshold(raw, unit); ok {
			g.Max, g.Source = v, "env "+thresholdEnv(metric)+"="+raw
		}
	}
	r.gates[metric] = g
}

// MaxErrorRate sets the run-wide error ceiling (default 0: any failed sample fails the run).
func (r *Run) MaxErrorRate(x float64) { r.maxErrorRate = x }

// Profile exposes the stamp for a caller that wants to log it. It is a copy.
func (r *Run) Profile() Profile { return r.profile }

func thresholdEnv(metric string) string {
	var b strings.Builder
	b.WriteString("PALAI_PERF_MAX_")
	for _, c := range strings.ToUpper(metric) {
		if (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func parseThreshold(raw, unit string) (float64, bool) {
	if unit == "ns" {
		if d, err := time.ParseDuration(raw); err == nil {
			return float64(d.Nanoseconds()), true
		}
	}
	v, err := strconv.ParseFloat(raw, 64)
	return v, err == nil
}

// Complete writes profile.json + samples.jsonl + summary.json into dir and applies the gate.
//
// Order matters and is the invariant: the profile is validated FIRST and nothing is written when it is
// incomplete (ErrNoProfile, zero artifacts). Percentiles are then derived from the samples that were
// just written, and the summary carries the samples digest so the derivation can be re-run against the
// bytes on disk.
func (r *Run) Complete(dir string) (Summary, error) {
	r.profile.FinishedAt = time.Now().UTC()
	if err := r.profile.validate(); err != nil {
		return Summary{}, err
	}
	if len(r.samples) == 0 {
		return Summary{}, fmt.Errorf("performance: %s recorded no samples", r.profile.Case)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Summary{}, fmt.Errorf("create output dir: %w", err)
	}

	var jsonl strings.Builder
	for _, s := range r.samples {
		line, err := json.Marshal(s)
		if err != nil {
			return Summary{}, fmt.Errorf("encode sample: %w", err)
		}
		jsonl.Write(line)
		jsonl.WriteByte('\n')
	}
	samplesPath := filepath.Join(dir, "samples.jsonl")
	if err := os.WriteFile(samplesPath, []byte(jsonl.String()), 0o644); err != nil {
		return Summary{}, fmt.Errorf("write samples: %w", err)
	}
	profileBytes, err := json.MarshalIndent(r.profile, "", "  ")
	if err != nil {
		return Summary{}, fmt.Errorf("encode profile: %w", err)
	}
	profilePath := filepath.Join(dir, "profile.json")
	if err := os.WriteFile(profilePath, append(profileBytes, '\n'), 0o644); err != nil {
		return Summary{}, fmt.Errorf("write profile: %w", err)
	}

	// Derive from the bytes just written, not from the in-memory slice, so the summary describes the
	// file a verifier will read.
	raw, err := os.ReadFile(samplesPath)
	if err != nil {
		return Summary{}, fmt.Errorf("read back samples: %w", err)
	}
	parsed, err := ParseSamples(raw)
	if err != nil {
		return Summary{}, err
	}
	sum := Summarize(parsed, r.gates, r.maxErrorRate)
	sum.Case = r.profile.Case
	sum.SamplesFile = "samples.jsonl"
	sum.SamplesSHA256 = digest(raw)
	sum.ProfileFile = "profile.json"
	sum.ProfileSHA256 = digest(append(profileBytes, '\n'))

	body, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return Summary{}, fmt.Errorf("encode summary: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), append(body, '\n'), 0o644); err != nil {
		return Summary{}, fmt.Errorf("write summary: %w", err)
	}
	if !sum.Pass {
		return sum, fmt.Errorf("%w: %s", ErrThreshold, strings.Join(sum.Failures, "; "))
	}
	return sum, nil
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ParseSamples reads samples.jsonl. It is exported because the whole point of writing raw samples is
// that another program (E18 T10's verifier) re-derives the percentiles from them.
func ParseSamples(raw []byte) ([]Sample, error) {
	var out []Sample
	for i, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var s Sample
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			return nil, fmt.Errorf("samples.jsonl line %d: %w", i+1, err)
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, errors.New("performance: samples.jsonl carries no samples")
	}
	return out, nil
}

// Summarize derives every metric's percentiles from raw samples and applies the gates. Exported for the
// same reason as ParseSamples: recompute-over-copy needs a shared, callable derivation.
func Summarize(samples []Sample, gates map[string]Threshold, maxErrorRate float64) Summary {
	byMetric := map[string][]Sample{}
	order := []string{}
	for _, s := range samples {
		if _, seen := byMetric[s.Metric]; !seen {
			order = append(order, s.Metric)
		}
		byMetric[s.Metric] = append(byMetric[s.Metric], s)
	}
	sort.Strings(order)

	sum := Summary{
		PercentileMethod: percentileMethod,
		SampleCount:      len(samples),
		MaxErrorRate:     maxErrorRate,
		DerivedFromRaw:   true,
		Pass:             true,
		NoSLOClaim:       true,
		// The stamp is the claim's negative space: these numbers were NOT taken on reference hardware.
		ReferenceHardware: false,
	}
	for _, metric := range order {
		group := byMetric[metric]
		values := make([]float64, 0, len(group))
		errCount := 0
		for _, s := range group {
			values = append(values, s.Value)
			if !s.OK {
				errCount++
			}
		}
		sort.Float64s(values)
		st := Stat{
			Metric:    metric,
			Unit:      group[0].Unit,
			Count:     len(group),
			Errors:    errCount,
			ErrorRate: float64(errCount) / float64(len(group)),
			Min:       values[0],
			P50:       percentile(values, 50),
			P95:       percentile(values, 95),
			P99:       percentile(values, 99),
			Max:       values[len(values)-1],
			Pass:      true,
		}
		if g, ok := gates[metric]; ok {
			gv := percentile(values, g.Percentile)
			st.Gate, st.GateValue = &g, gv
			if gv > g.Max {
				st.Pass = false
				sum.Failures = append(sum.Failures, fmt.Sprintf("%s p%d = %s exceeds %s",
					metric, g.Percentile, format(gv, st.Unit), format(g.Max, st.Unit)))
			}
		}
		if st.ErrorRate > maxErrorRate {
			st.Pass = false
			sum.Failures = append(sum.Failures, fmt.Sprintf("%s error rate %.3f exceeds %.3f",
				metric, st.ErrorRate, maxErrorRate))
		}
		if !st.Pass {
			sum.Pass = false
		}
		sum.Stats = append(sum.Stats, st)
	}
	// A gate configured for a metric that produced NO samples is a silently vacuous gate — the exact
	// shape of a harness that "passes" because it measured nothing.
	for metric, g := range gates {
		if _, ok := byMetric[metric]; !ok {
			sum.Pass = false
			sum.Failures = append(sum.Failures,
				fmt.Sprintf("%s has a p%d gate but produced no samples (vacuous gate)", metric, g.Percentile))
		}
	}
	sort.Strings(sum.Failures)
	return sum
}

// percentile is the nearest-rank percentile of an ASCENDING-sorted slice, per percentileMethod.
func percentile(sorted []float64, pct int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(float64(pct)/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func format(v float64, unit string) string {
	if unit == "ns" {
		return time.Duration(int64(v)).String()
	}
	return strconv.FormatFloat(v, 'f', -1, 64) + " " + unit
}

// detectProfile describes the machine. Every probe must succeed: a machine that cannot be described
// cannot host a comparable number, and Docker Desktop's version is part of the description because the
// stack under test runs on it.
func detectProfile() (Profile, error) {
	p := Profile{Arch: runtime.GOARCH, Cores: runtime.NumCPU(), Go: runtime.Version()}

	osName, err := run("uname", "-sr")
	if err != nil {
		return p, fmt.Errorf("read os version: %w", err)
	}
	p.OS = osName

	switch runtime.GOOS {
	case "darwin":
		if p.Machine, err = run("sysctl", "-n", "hw.model"); err != nil {
			return p, fmt.Errorf("read machine model: %w", err)
		}
		mem, err := run("sysctl", "-n", "hw.memsize")
		if err != nil {
			return p, fmt.Errorf("read memory size: %w", err)
		}
		if p.MemoryBytes, err = strconv.ParseInt(mem, 10, 64); err != nil {
			return p, fmt.Errorf("parse memory size %q: %w", mem, err)
		}
	default:
		if p.Machine, err = run("uname", "-m"); err != nil {
			return p, fmt.Errorf("read machine: %w", err)
		}
		info, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return p, fmt.Errorf("read /proc/meminfo: %w", err)
		}
		for _, line := range strings.Split(string(info), "\n") {
			if !strings.HasPrefix(line, "MemTotal:") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, err := strconv.ParseInt(fields[1], 10, 64)
				if err != nil {
					return p, fmt.Errorf("parse MemTotal %q: %w", fields[1], err)
				}
				p.MemoryBytes = kb * 1024
			}
		}
		if p.MemoryBytes <= 0 {
			return p, errors.New("no MemTotal in /proc/meminfo")
		}
	}

	// The numbers are Docker-Desktop-profile numbers, so the daemon identity is part of the profile —
	// not decoration. No daemon, no comparable number, no result.
	docker, err := run("docker", "version", "--format", "{{.Server.Platform.Name}} (engine {{.Server.Version}}, {{.Server.Os}}/{{.Server.Arch}})")
	if err != nil {
		return p, fmt.Errorf("read docker version: %w", err)
	}
	p.Docker = docker
	return p, nil
}

func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", fmt.Errorf("%s returned nothing", name)
	}
	return s, nil
}

// OutputDir is the per-case artifact directory: PALAI_PERF_OUT/<case> when the runner set it (the
// script does), otherwise a temp directory. E18 T10 consumes profile.json + samples.jsonl from here.
func OutputDir(caseID string) string {
	base := os.Getenv("PALAI_PERF_OUT")
	if base == "" {
		base = filepath.Join(os.TempDir(), "palai-perf")
	}
	return filepath.Join(base, caseID)
}
