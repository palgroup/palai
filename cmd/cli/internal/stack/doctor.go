package stack

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/jackc/pgx/v5"
)

// Digest pins doctor verifies against the running containers. postgres matches CI
// (.github/workflows/ci.yml); seaweedfs matches ADR-0004. The control-plane, runner, and
// engine are locally built, so doctor labels them rather than pinning (release digest
// pinning is E18).
const (
	postgresDigest = "sha256:17e67d7b9890c99b055ba1e0d5c5be4ec27c9d3a72bda32db24a5e5d8a85af0c"
	seaweedDigest  = "sha256:c7d6c721b30ae711db766bbbfd40192776e263d4e51e22f57baef7bef93c12c6"
	// currentSchemaVersion is the highest embedded migration (storage/migrations). Bump
	// alongside a new migration.
	currentSchemaVersion = 2
)

// Check is one doctor result: a status ("ok" is green) and a human detail.
type Check struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// Report is the doctor --json contract: an overall verdict plus the check map.
type Report struct {
	OK     bool             `json:"ok"`
	Checks map[string]Check `json:"checks"`
}

// Doctor runs the nine local-stack checks and reports them. With jsonOut it prints the
// report as JSON and always exits 0 (the verdict is in the body, which the e2e harness
// parses). Human output prints a table and returns an error when any check is not green,
// so `scripts/local/doctor` exits non-zero on an unhealthy stack.
func Doctor(jsonOut bool) error {
	cfg, p, err := loadConfig()
	if err != nil {
		return err
	}
	report := runChecks(cfg, p)

	if jsonOut {
		raw, err := json.Marshal(report)
		if err != nil {
			return err
		}
		fmt.Println(string(raw))
		return nil
	}
	for name, c := range report.Checks {
		fmt.Printf("%-18s %-8s %s\n", name, c.Status, c.Detail)
	}
	if !report.OK {
		return fmt.Errorf("doctor: one or more checks are not green")
	}
	fmt.Println("all checks green")
	return nil
}

// runChecks executes every check and folds them into a report. OK is true only when all
// statuses are "ok".
func runChecks(cfg Config, p paths) Report {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	caps, apiCheck := checkAPI(ctx, cfg, p)
	pgURL := pgURLFor(cfg, p)

	checks := map[string]Check{
		"api":               apiCheck,
		"migration":         checkMigration(ctx, pgURL),
		"object_store":      checkObjectStore(ctx, cfg),
		"runner":            checkRunner(cfg, p),
		"image_digests":     checkImageDigests(ctx, cfg),
		"provider":          checkProvider(p),
		"clock":             checkClock(ctx, pgURL),
		"retention_ttl":     checkRetention(caps),
		"runner_tls_reject": checkRunnerTLSReject(cfg, p),
		"supervisor":        checkSupervisor(ctx, cfg),
		"host_quarantine":   checkQuarantine(ctx, pgURL),
	}
	ok := true
	for _, c := range checks {
		if c.Status != "ok" {
			ok = false
		}
	}
	return Report{OK: ok, Checks: checks}
}

// capabilities is the slice of GET /v1/capabilities doctor reads for the api and
// retention_ttl checks.
type capabilities struct {
	Retention struct {
		StoreFalseTTLSeconds int `json:"store_false_ttl_seconds"`
	} `json:"retention"`
}

// checkAPI proves the public API answers GET /v1/capabilities 200 with the bootstrap key,
// and returns the decoded body so retention_ttl reflects the same discovery surface.
func checkAPI(ctx context.Context, cfg Config, p paths) (capabilities, Check) {
	var caps capabilities
	key, err := readTrimmed(p.apiKey)
	if err != nil {
		return caps, fail("api key unreadable: " + err.Error())
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+"/v1/capabilities", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return caps, fail("GET /v1/capabilities: " + err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return caps, fail(fmt.Sprintf("GET /v1/capabilities = %d, want 200", resp.StatusCode))
	}
	_ = json.NewDecoder(resp.Body).Decode(&caps)
	return caps, ok(fmt.Sprintf("GET /v1/capabilities 200 at %s", cfg.BaseURL))
}

// checkMigration confirms the durable schema is at the current version.
func checkMigration(ctx context.Context, pgURL string) Check {
	conn, err := pgx.Connect(ctx, pgURL)
	if err != nil {
		return fail("connect Postgres: " + err.Error())
	}
	defer conn.Close(ctx)
	var version int
	if err := conn.QueryRow(ctx, "SELECT coalesce(max(version), 0) FROM schema_migrations").Scan(&version); err != nil {
		return fail("read schema_migrations: " + err.Error())
	}
	if version < currentSchemaVersion {
		return fail(fmt.Sprintf("schema at version %d, want %d", version, currentSchemaVersion))
	}
	return ok(fmt.Sprintf("schema at version %d (current)", version))
}

// checkObjectStore pings the SeaweedFS S3 endpoint with aws-sdk-go-v2. A structured S3
// error still proves the endpoint speaks S3 (reachable); only a transport failure means
// the object store is down.
func checkObjectStore(ctx context.Context, cfg Config) Check {
	endpoint := fmt.Sprintf("http://127.0.0.1:%d", cfg.S3Port)
	client := s3.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: staticCreds{}}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	out, err := client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err == nil {
		return ok(fmt.Sprintf("S3 endpoint reachable at %s (%d buckets)", endpoint, len(out.Buckets)))
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return ok(fmt.Sprintf("S3 endpoint reachable at %s (responded %s)", endpoint, apiErr.ErrorCode()))
	}
	return fail("reach S3 endpoint: " + err.Error())
}

// checkRunner proves the runner gateway's mTLS listener is up: a TLS handshake trusting
// the local CA and pinning the controller DNS completes (the server certificate verifies).
func checkRunner(cfg Config, p paths) Check {
	pool, err := caPool(p)
	if err != nil {
		return fail(err.Error())
	}
	dialer := &tls.Dialer{Config: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: cfg.ControllerDNS}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialer.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", cfg.RunnerPort))
	if err != nil {
		return fail(fmt.Sprintf("runner gateway TLS handshake on :%d: %v", cfg.RunnerPort, err))
	}
	_ = conn.Close()
	return ok(fmt.Sprintf("runner gateway TLS listener up at :%d (server cert verified)", cfg.RunnerPort))
}

// checkImageDigests verifies the two external bases run at their pinned digests and the
// three locally-built images are present.
func checkImageDigests(ctx context.Context, cfg Config) Check {
	postgresImg := inspectContainerImage(ctx, cfg.Project+"-postgres-1")
	if !strings.Contains(postgresImg, postgresDigest) {
		return fail("postgres not at pinned digest: " + postgresImg)
	}
	objectImg := inspectContainerImage(ctx, cfg.Project+"-object-store-1")
	if !strings.Contains(objectImg, seaweedDigest) {
		return fail("object-store not at pinned digest: " + objectImg)
	}
	if !imageExists(ctx, "palai/control-plane:local") || !imageExists(ctx, "palai/runner:local") {
		return fail("locally-built control-plane/runner image missing")
	}
	if !imageExists(ctx, engineImage) {
		return fail("locally-built engine image missing: " + engineImage)
	}
	return ok("postgres+object-store pinned; control-plane/runner/engine locally-built")
}

// checkProvider verifies the provider secret slot: configured (non-empty, 0600) is green,
// and so is an empty slot — a fresh stack has no provider and doctor stays green.
func checkProvider(p paths) Check {
	path := p.secretPath("provider-one")
	info, err := os.Stat(path)
	if err != nil {
		return fail("provider secret slot missing: " + err.Error())
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		return fail(fmt.Sprintf("provider secret mode %o, want 600", perm))
	}
	if info.Size() == 0 {
		return ok("no provider configured (add with `palai provider add provider-one`)")
	}
	return ok("provider-one configured (0600)")
}

// checkClock compares the database clock to the host clock; a skew under two seconds keeps
// idempotency and lease fencing honest.
func checkClock(ctx context.Context, pgURL string) Check {
	conn, err := pgx.Connect(ctx, pgURL)
	if err != nil {
		return fail("connect Postgres: " + err.Error())
	}
	defer conn.Close(ctx)
	host := time.Now()
	var dbTime time.Time
	if err := conn.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&dbTime); err != nil {
		return fail("read db clock: " + err.Error())
	}
	skew := dbTime.Sub(host)
	if skew < 0 {
		skew = -skew
	}
	if skew >= 2*time.Second {
		return fail(fmt.Sprintf("db clock skew %s exceeds 2s", skew.Round(time.Millisecond)))
	}
	return ok(fmt.Sprintf("db clock within %s of host", skew.Round(time.Millisecond)))
}

// checkQuarantine surfaces hosts quarantined by an allocation-destroy failure (spec §29 SAN-008, E10
// T6). Zero quarantined hosts is green; a non-zero count is still green but NAMED in the detail so an
// operator sees a poisoned host refusing new placement. Only an unreachable DB or a missing table fails.
func checkQuarantine(ctx context.Context, pgURL string) Check {
	conn, err := pgx.Connect(ctx, pgURL)
	if err != nil {
		return fail("connect Postgres: " + err.Error())
	}
	defer conn.Close(ctx)
	var count int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM host_quarantine").Scan(&count); err != nil {
		return fail("read host_quarantine: " + err.Error())
	}
	if count == 0 {
		return ok("no quarantined hosts")
	}
	return ok(fmt.Sprintf("%d host(s) quarantined — new placement refused there", count))
}

// checkRetention reflects the configured store:false retention TTL discovery publishes.
func checkRetention(caps capabilities) Check {
	ttl := caps.Retention.StoreFalseTTLSeconds
	if ttl == 0 {
		return ok("store:false retention disabled (ttl=0)")
	}
	return ok(fmt.Sprintf("store:false retention ttl=%ds", ttl))
}

// checkSupervisor surfaces the control-plane background-loop restart counters over the
// unauthenticated /healthz/supervisor endpoint (H2). Zero restarts on a healthy boot is
// green; a non-zero count is still green but named in the detail so an operator sees a loop
// that has been restarting. Only an unreachable or unparseable endpoint fails.
func checkSupervisor(ctx context.Context, cfg Config) Check {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+"/healthz/supervisor", nil)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return fail("GET /healthz/supervisor: " + err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fail(fmt.Sprintf("GET /healthz/supervisor = %d, want 200", resp.StatusCode))
	}
	var body struct {
		Restarts map[string]int `json:"restarts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fail("decode /healthz/supervisor: " + err.Error())
	}
	total := 0
	for _, n := range body.Restarts {
		total += n
	}
	if total == 0 {
		return ok("background loops supervised (0 restarts)")
	}
	return ok(fmt.Sprintf("background loops supervised (%d restarts: %v)", total, body.Restarts))
}

// checkRunnerTLSReject proves the connect endpoint enforces mutual TLS: a client trusting
// the CA but presenting no runner certificate is rejected with 401.
func checkRunnerTLSReject(cfg Config, p paths) Check {
	pool, err := caPool(p)
	if err != nil {
		return fail(err.Error())
	}
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: cfg.ControllerDNS}},
	}
	resp, err := client.Get(fmt.Sprintf("https://127.0.0.1:%d/v1/runner/connect", cfg.RunnerPort))
	if err != nil {
		return fail("certless connect probe errored before a status: " + err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		return fail(fmt.Sprintf("certless /v1/runner/connect = %d, want 401", resp.StatusCode))
	}
	return ok("certless /v1/runner/connect rejected (401)")
}

// --- helpers ---

func ok(detail string) Check   { return Check{Status: "ok", Detail: detail} }
func fail(detail string) Check { return Check{Status: "fail", Detail: detail} }

// staticCreds is a fixed credential provider for the SeaweedFS S3 ping; the local object
// store has no configured identities, so the exact values are immaterial.
type staticCreds struct{}

func (staticCreds) Retrieve(context.Context) (aws.Credentials, error) {
	return aws.Credentials{AccessKeyID: "palai", SecretAccessKey: "palai", Source: "palai-doctor"}, nil
}

// pgURLFor builds the host Postgres URL from config and the minted pg password; an
// unreadable password yields an obviously-invalid URL so the pg checks fail with detail
// rather than panicking.
func pgURLFor(cfg Config, p paths) string {
	password, err := readTrimmed(p.pgPassword)
	if err != nil {
		return "postgres://palai@127.0.0.1:0/palai"
	}
	return cfg.databaseURL(password)
}

// caPool loads the local CA trust anchor for the runner-port probes.
func caPool(p paths) (*x509.CertPool, error) {
	pem, err := os.ReadFile(p.caCert)
	if err != nil {
		return nil, fmt.Errorf("read CA certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("CA certificate file held no certificates")
	}
	return pool, nil
}

// inspectContainerImage returns a container's configured image reference (carrying the
// digest for a digest-pinned service), or "" when the container is absent.
func inspectContainerImage(ctx context.Context, name string) string {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.Config.Image}}", name)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}

// imageExists reports whether a local image reference resolves.
func imageExists(ctx context.Context, ref string) bool {
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", ref)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}
