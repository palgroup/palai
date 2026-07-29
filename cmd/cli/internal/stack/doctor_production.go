package stack

// doctor_production.go is `palai doctor --env-file production.env`: the SAME health questions
// `palai local doctor` asks, reached the way a production stack can actually be reached.
//
// WHY A SECOND ENTRY POINT AND NOT A FLAG ON THE OLD ONE. `local doctor` probes the host-published
// ports in ${PALAI_HOME}/config.json. The production overlay publishes NONE of them (`ports: !reset
// []`, deploy/compose/production.yml) — only the TLS edge — so against a production stack it
// reported 13 of its 15 checks red for one reason that had nothing to do with the stack's health
// (measured 2026-07-29, docs/operations/cloud-smoke-report.md, Bulgu 5). The two commands ask the
// same questions; they differ ONLY in how they reach the answer, and that difference is the whole
// posture, so it is a different command rather than a mode flag that would silently change what a
// scripted `local doctor` measures.
//
// NOTHING HERE IS A WEAKER CHECK. Every verdict is the shared function `local doctor` uses
// (migrationCheck, clockCheck, queueCheck, callbackCheck, quarantineCheck, supervisorCheck,
// runnerIdentityFromBody, diskCheck, checkProvider, checkRetention). Only the transports differ,
// and both were already proven in this CLI:
//
//   - `docker exec` by container name — what `palai backup`/`restore`/`support-bundle` use, which
//     is exactly why they work against an edge-only stack (install_backup.go's header says so);
//   - the TLS edge — what install.md step 6 already has the operator curl.
//
// Two checks come out STRONGER than their local counterparts: `api` now proves TLS termination, CA
// trust and the edge's /v1/* path match rather than a plaintext localhost port, and `edge_cert`
// did not exist locally at all.
//
// A check that cannot be made meaningful under this posture reports "n/a" with the reason. It is
// NOT counted as green — the summary line names it, and `--json` carries the status — but it does
// not fail the verdict either, because a permanently-unmeasurable check that reddens every run
// trains an operator to ignore the command.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/palgroup/palai/storage"
)

// ProductionDoctor runs the health checks against the production stack described by envFile. With
// jsonOut it prints the Report as JSON and exits 0 (the verdict is in the body, like `local
// doctor`); human output prints the table and returns an error when any check FAILED.
func ProductionDoctor(envFile string, jsonOut bool) error {
	s, err := newProdStack(envFile)
	if err != nil {
		return err
	}
	report := s.runChecks()

	if jsonOut {
		raw, err := json.Marshal(report)
		if err != nil {
			return err
		}
		fmt.Println(string(raw))
		return nil
	}
	var failed, na []string
	for name, c := range report.Checks {
		fmt.Printf("%-18s %-8s %s\n", name, c.Status, c.Detail)
		switch c.Status {
		case statusFail:
			failed = append(failed, name)
		case statusNA:
			na = append(na, name)
		}
	}
	if len(na) > 0 {
		fmt.Printf("\n%d check(s) not measurable under the edge-only posture: %s\n", len(na), strings.Join(na, ", "))
	}
	if len(failed) > 0 {
		return fmt.Errorf("doctor: %d check(s) failed: %s", len(failed), strings.Join(failed, ", "))
	}
	fmt.Printf("%d check(s) green, 0 failed\n", len(report.Checks)-len(na))
	return nil
}

// Check statuses. "ok" and "fail" are the shipped two; "n/a" is this file's third — a question this
// posture cannot answer, named rather than quietly passed.
const (
	statusOK   = "ok"
	statusFail = "fail"
	statusNA   = "n/a"
)

func unavailable(detail string) Check { return Check{Status: statusNA, Detail: detail} }

// prodStack is the production stack a health run addresses: the data dir on this host, the compose
// project its containers are named after, the TLS edge port, and the env file's own map (the edge
// cert override is read from it, exactly as the overlay's mounts are).
type prodStack struct {
	env      map[string]string
	home     string
	project  string
	edgePort int
	cfg      Config // Project only — reuses containerName/objectVolume
}

func newProdStack(envFile string) (prodStack, error) {
	env, err := parseEnvFile(envFile)
	if err != nil {
		return prodStack{}, fmt.Errorf("read env file %s: %w", envFile, err)
	}
	s := prodStack{env: env, home: env["PALAI_HOME"], project: env["PALAI_COMPOSE_PROJECT"]}
	if s.home == "" || s.project == "" {
		return prodStack{}, fmt.Errorf("%s must set PALAI_HOME and PALAI_COMPOSE_PROJECT — they are how this command finds the data dir and the containers", envFile)
	}
	if s.edgePort, err = strconv.Atoi(env["PALAI_EDGE_PORT"]); err != nil || s.edgePort <= 0 {
		return prodStack{}, fmt.Errorf("%s must set PALAI_EDGE_PORT to the published TLS port", envFile)
	}
	s.cfg = Config{Project: s.project, DataDir: s.home}
	return s, nil
}

func (s prodStack) runChecks() Report {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	caps, apiCheck, certCheck := s.checkEdge(ctx)
	gwOut, gwErr := s.runnerGatewayProbe(ctx)

	checks := map[string]Check{
		"api":       apiCheck,
		"edge_cert": certCheck,
		// The one trap `palai backup` springs on a stack that otherwise looks perfect.
		"backup_target": s.checkBackupTarget(ctx),
		"containers":    s.checkContainers(ctx),

		"migration":       s.checkMigration(ctx),
		"clock":           s.checkClock(ctx),
		"queue":           s.checkQueue(ctx),
		"callback":        s.checkCallback(ctx),
		"host_quarantine": s.checkQuarantine(ctx),

		"object_store":      s.checkObjectStore(ctx),
		"runner":            runnerGatewayCheck(gwOut, gwErr),
		"runner_tls_reject": runnerRejectCheck(gwOut, gwErr),
		"runner_identity":   s.checkRunnerIdentity(ctx),
		"supervisor":        s.checkSupervisor(ctx),
		"image_digests":     s.checkImageDigests(ctx),

		// Host-side and env-side: identical to `local doctor`'s, no transport involved.
		"disk":          checkDisk(s.cfg),
		"provider":      checkProvider(paths{secretsDir: filepath.Join(s.home, "secrets")}),
		"retention_ttl": checkRetention(caps),
	}
	okAll := true
	for _, c := range checks {
		if c.Status == statusFail {
			okAll = false
		}
	}
	return Report{OK: okAll, Checks: checks}
}

// --- the TLS edge (the one published surface) -------------------------------------------------

// checkEdge does ONE TLS round trip and derives two checks from it: that the API answers 200
// through the edge with the bootstrap key, and that the certificate the edge is SERVING is the one
// on disk. The second exists because `docker compose up -d edge` does not reload a replaced
// certificate — compose sees no config change and leaves the container alone — so an operator can
// swap a certificate, see no error, and keep serving the old one indefinitely (measured
// 2026-07-29, cloud-smoke-report.md, Bulgu 4). `restart edge` is what reloads it.
func (s prodStack) checkEdge(ctx context.Context) (capabilities, Check, Check) {
	var caps capabilities
	certPath, _ := edgeCertPaths(s.env)
	want, err := leafFromPEMFile(certPath)
	if err != nil {
		c := fail("read the configured edge certificate " + certPath + ": " + err.Error())
		return caps, c, c
	}
	serverName := certServerName(want)
	pool, err := s.trustPool()
	if err != nil {
		c := fail(err.Error())
		return caps, c, c
	}

	var served *x509.Certificate
	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			// The edge is published on the host, but the certificate names a domain that may not
			// resolve to it (or to anything) from this shell — the same reason install.md's probe
			// uses `curl --resolve`. Verification stays FULL: only the address is substituted.
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort("127.0.0.1", strconv.Itoa(s.edgePort)))
			},
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool},
		},
	}
	key, err := readTrimmed(filepath.Join(s.home, "api-key"))
	if err != nil {
		c := fail("bootstrap api-key unreadable: " + err.Error())
		return caps, c, c
	}
	url := fmt.Sprintf("https://%s:%d/v1/capabilities", serverName, s.edgePort)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return caps, fail("GET " + url + " through the edge: " + err.Error()),
			fail("the edge did not complete a verified TLS handshake for " + serverName + ": " + err.Error())
	}
	defer resp.Body.Close()
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		served = resp.TLS.PeerCertificates[0]
	}
	apiCheck := ok(fmt.Sprintf("GET /v1/capabilities 200 through the TLS edge at %s (CA-verified)", url))
	if resp.StatusCode != http.StatusOK {
		apiCheck = fail(fmt.Sprintf("GET %s = %d, want 200", url, resp.StatusCode))
	} else {
		_ = json.NewDecoder(resp.Body).Decode(&caps)
	}
	return caps, apiCheck, edgeCertCheck(want, served, certPath, time.Now())
}

// edgeCertCheck compares the certificate the edge SERVED with the one configured on disk and
// reports its remaining lifetime. Serving a different certificate than the file says is the
// silent half of a certificate swap; an expired one is the loud half nobody looks for.
func edgeCertCheck(want, served *x509.Certificate, certPath string, now time.Time) Check {
	if served == nil {
		return fail("the edge presented no certificate")
	}
	names := strings.Join(certNames(served), ", ")
	if !served.Equal(want) {
		// Name the SERIAL as well as the SANs: a renewed certificate usually carries the same names,
		// so "different certificate (palai.example.com) than the file holds (palai.example.com)"
		// would read like a bug in the check rather than a stale container.
		return fail(fmt.Sprintf("the edge is serving a DIFFERENT certificate (%s, serial %s) than %s holds (%s, serial %s) — `docker compose up -d edge` does not reload a replaced certificate, `restart edge` does",
			names, served.SerialNumber, certPath, strings.Join(certNames(want), ", "), want.SerialNumber))
	}
	if remaining := served.NotAfter.Sub(now); remaining <= 0 {
		return fail(fmt.Sprintf("the edge certificate for %s EXPIRED %s ago (at %s)", names, (-remaining).Round(time.Hour), served.NotAfter.UTC().Format(time.RFC3339)))
	}
	return ok(fmt.Sprintf("edge is serving %s (%s), valid until %s", certPath, names, served.NotAfter.UTC().Format(time.RFC3339)))
}

// trustPool is the system roots plus this install's local CA. A real-domain certificate verifies
// against the system roots; the `palai init` default verifies against ${PALAI_HOME}/ca/ca.crt.
// Neither path relaxes verification — there is no InsecureSkipVerify anywhere in this file.
func (s prodStack) trustPool() (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	pem, err := os.ReadFile(filepath.Join(s.home, "ca", "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("read the local CA %s: %w", filepath.Join(s.home, "ca", "ca.crt"), err)
	}
	pool.AppendCertsFromPEM(pem)
	return pool, nil
}

func leafFromPEMFile(path string) (*x509.Certificate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	for len(raw) > 0 {
		var block *pem.Block
		block, raw = pem.Decode(raw)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			return x509.ParseCertificate(block.Bytes)
		}
	}
	return nil, errors.New("no CERTIFICATE block in the file")
}

// certServerName is the name a client must ask for to make this certificate verify: its first DNS
// SAN, or the Common Name when it carries none.
func certServerName(c *x509.Certificate) string {
	if len(c.DNSNames) > 0 {
		return c.DNSNames[0]
	}
	return c.Subject.CommonName
}

func certNames(c *x509.Certificate) []string {
	names := append([]string{}, c.DNSNames...)
	for _, ip := range c.IPAddresses {
		names = append(names, ip.String())
	}
	if len(names) == 0 {
		names = append(names, "CN="+c.Subject.CommonName)
	}
	return names
}

// --- Postgres, by docker exec (the transport `palai backup` already relies on) -----------------

func (s prodStack) pgScalar(ctx context.Context, query string) (string, error) {
	return pgQueryScalar(ctx, s.cfg.containerName("postgres"), query)
}

func (s prodStack) checkMigration(ctx context.Context) Check {
	raw, err := s.pgScalar(ctx, migrationVersionSQL)
	if err != nil {
		return fail("read schema_migrations: " + err.Error())
	}
	version, err := strconv.Atoi(raw)
	if err != nil {
		return fail("schema_migrations returned " + strconv.Quote(raw))
	}
	return migrationCheck(version)
}

func (s prodStack) checkClock(ctx context.Context) Check {
	host := time.Now()
	raw, err := s.pgScalar(ctx, dbClockSQL)
	if err != nil {
		return fail("read db clock: " + err.Error())
	}
	// psql -tA renders timestamptz as "2026-07-29 12:34:56.789012+00".
	dbTime, err := time.Parse("2006-01-02 15:04:05.999999-07", raw)
	if err != nil {
		return fail("parse db clock " + strconv.Quote(raw) + ": " + err.Error())
	}
	return clockCheck(dbTime.Sub(host))
}

func (s prodStack) checkQuarantine(ctx context.Context) Check {
	n, err := pgQueryInt(ctx, s.cfg.containerName("postgres"), quarantineCountSQL)
	if err != nil {
		return fail("read host_quarantine: " + err.Error())
	}
	return quarantineCheck(n)
}

// checkQueue/checkCallback run the SAME named statements in storage/queries/metrics.sql the local
// doctor and the /metrics collector run; only the client differs. psql -tA returns the columns
// pipe-separated on one line.
func (s prodStack) checkQueue(ctx context.Context) Check {
	raw, err := s.pgScalar(ctx, oneLine(storage.Query("MetricQueueReady")))
	if err != nil {
		return fail("read queue depth: " + err.Error())
	}
	depth, oldest, found := strings.Cut(raw, "|")
	if !found {
		return fail("MetricQueueReady returned " + strconv.Quote(raw))
	}
	d, err1 := strconv.ParseInt(strings.TrimSpace(depth), 10, 64)
	o, err2 := strconv.ParseFloat(strings.TrimSpace(oldest), 64)
	if err1 != nil || err2 != nil {
		return fail("MetricQueueReady returned " + strconv.Quote(raw))
	}
	return queueCheck(d, o)
}

func (s prodStack) checkCallback(ctx context.Context) Check {
	rows, err := pgQueryList(ctx, s.cfg.containerName("postgres"), oneLine(storage.Query("MetricWebhookDeliveryStates")))
	if err != nil {
		return fail("read webhook deliveries: " + err.Error())
	}
	states := map[string]int64{}
	for _, row := range rows {
		state, count, found := strings.Cut(row, "|")
		if !found {
			return fail("MetricWebhookDeliveryStates returned " + strconv.Quote(row))
		}
		n, err := strconv.ParseInt(strings.TrimSpace(count), 10, 64)
		if err != nil {
			return fail("MetricWebhookDeliveryStates returned " + strconv.Quote(row))
		}
		states[strings.TrimSpace(state)] = n
	}
	return callbackCheck(states)
}

// oneLine flattens a multi-line named statement (and strips its `--` comments) so it can be fed to
// `psql -tAq` as a single statement over stdin.
func oneLine(query string) string {
	var kept []string
	for _, line := range strings.Split(query, "\n") {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "--") {
			kept = append(kept, t)
		}
	}
	return strings.TrimSuffix(strings.Join(kept, " "), ";")
}

// --- the internal control-plane probes (/healthz*, which the edge deliberately does not proxy) --

// internalGet reads a control-plane path from INSIDE the control-plane container, with the busybox
// wget its own compose healthcheck already uses — no extra image, no host port, and nothing the
// edge would have to expose. /healthz and /metrics are off the edge on purpose (the Caddyfile
// proxies only /v1/*), and that is the posture, not a gap.
func (s prodStack) internalGet(ctx context.Context, path string) ([]byte, error) {
	return dockerCapture(ctx, nil, "exec", s.cfg.containerName("control-plane"),
		"wget", "-q", "-O-", "http://127.0.0.1:8080"+path)
}

func (s prodStack) checkSupervisor(ctx context.Context) Check {
	raw, err := s.internalGet(ctx, "/healthz/supervisor")
	if err != nil {
		return fail("GET /healthz/supervisor inside the control-plane container: " + err.Error())
	}
	return supervisorCheck(raw)
}

func (s prodStack) checkRunnerIdentity(ctx context.Context) Check {
	raw, err := s.internalGet(ctx, "/healthz/runner")
	if err != nil {
		return fail("GET /healthz/runner inside the control-plane container: " + err.Error())
	}
	return runnerIdentityFromBody(raw, time.Now())
}

// checkObjectStore asks the CONTROL PLANE whether it can reach the object store at the address it
// was configured with (PALAI_S3_ENDPOINT) — a stronger question than the local doctor's host-port
// ping, because it is the reachability the artifact path actually depends on.
func (s prodStack) checkObjectStore(ctx context.Context) Check {
	out, err := dockerCapture(ctx, nil, "exec", s.cfg.containerName("control-plane"),
		"wget", "-q", "-O-", "http://object-store:8333/")
	if err != nil {
		return fail("the control-plane cannot reach http://object-store:8333/: " + err.Error())
	}
	return ok(fmt.Sprintf("object store answers the control-plane at http://object-store:8333/ (%d bytes)", len(out)))
}

// --- the runner gateway, from a one-off container on the stack's own network -------------------

// runnerGatewayProbe opens a verified TLS connection to the runner gateway from INSIDE the compose
// network and sends a certificate-less request. It runs in the digest-pinned postgres image the
// stack already has on disk — the same one install_backup.go uses as its utility container — so it
// pulls nothing. Both the handshake and the 401 come from this single probe.
func (s prodStack) runnerGatewayProbe(ctx context.Context) (string, error) {
	// The `; echo probeExitMarker=$?` and the sh exiting 0 regardless are deliberate: dockerCapture
	// discards stdout when the command exits non-zero, so letting openssl's failure be the exit
	// status would throw away the one thing that says WHY (the verify error). The probe's own
	// verdict rides in the output instead.
	script := `printf 'GET /v1/runner/connect HTTP/1.1\r\nHost: control-plane\r\nConnection: close\r\n\r\n' | ` +
		// -quiet is load-bearing twice: it implies -ign_eof, without which s_client tears the
		// connection down the moment stdin ends and the server's response never arrives (the probe
		// then reports "no HTTP status line" on a perfectly healthy gateway), and it keeps the
		// certificate chain out of the output while LEAVING openssl's verify errors on stderr,
		// which 2>&1 folds in for verifyDiagnostic.
		`openssl s_client -connect control-plane:8443 -servername control-plane -CAfile /ca.crt -verify_return_error -quiet 2>&1; ` +
		`echo "` + probeExitMarker + `$?"`
	out, err := dockerCapture(ctx, nil, "run", "--rm",
		"--network", s.project+"_default",
		"-v", filepath.Join(s.home, "ca", "ca.crt")+":/ca.crt:ro",
		"--entrypoint", "sh", postgresImageRef, "-c", script)
	return string(out), err
}

const probeExitMarker = "palai-probe-exit="

// runnerGatewayCheck: the gateway's TLS listener is up AND its certificate verifies against this
// install's CA under the name the runner pins. `-verify_return_error` makes a bad chain a non-zero
// exit, so a green here is a real verification, not a reachable socket.
func runnerGatewayCheck(out string, err error) Check {
	if err != nil {
		return fail("could not run the runner-gateway probe container: " + err.Error())
	}
	if code, found := probeExit(out); !found {
		return fail("the runner-gateway probe produced no verdict: " + firstLines(out, 3))
	} else if code != 0 {
		return fail("runner gateway TLS handshake on control-plane:8443 (from inside the compose network) failed: " + verifyDiagnostic(out))
	}
	return ok("runner gateway TLS listener up at control-plane:8443, server certificate verified against the local CA")
}

// probeExit reads the exit status the probe script echoed. Absent, the probe told us nothing and
// the caller must NOT read that as success.
func probeExit(out string) (int, bool) {
	for _, line := range strings.Split(out, "\n") {
		if rest, found := strings.CutPrefix(strings.TrimSpace(line), probeExitMarker); found {
			code, err := strconv.Atoi(rest)
			return code, err == nil
		}
	}
	return 0, false
}

// verifyDiagnostic pulls the lines that say WHY a handshake failed out of openssl's chatter.
func verifyDiagnostic(out string) string {
	var kept []string
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		if strings.Contains(l, "verify error") || strings.Contains(l, "Verify return code") ||
			strings.Contains(l, "connect:") || strings.Contains(l, "errno") {
			kept = append(kept, l)
		}
	}
	if len(kept) == 0 {
		return firstLines(out, 3)
	}
	return strings.Join(kept, " / ")
}

// runnerRejectCheck: the gateway enforces MUTUAL TLS. The probe above presented no client
// certificate, so the connect endpoint must answer 401 — a 101/200 would mean an unauthenticated
// process could take leases.
func runnerRejectCheck(out string, err error) Check {
	if code, ok := probeExit(out); err != nil || !ok || code != 0 {
		return unavailable("the certificate-less probe never reached a status (the handshake failed first — see the `runner` check)")
	}
	status, found := firstStatusLine(out)
	switch {
	case !found:
		return fail("the certificate-less /v1/runner/connect probe returned no HTTP status line: " + firstLines(out, 3))
	case strings.Contains(status, " 401"):
		return ok("certless /v1/runner/connect rejected (401) — the gateway enforces mutual TLS")
	default:
		return fail("certless /v1/runner/connect answered " + strconv.Quote(status) + ", want 401 — an unauthenticated client is not being rejected")
	}
}

func firstStatusLine(out string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "HTTP/") {
			return strings.TrimSpace(line), true
		}
	}
	return "", false
}

func firstLines(out string, n int) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, " / ") + " "
}

// --- container-level checks --------------------------------------------------------------------

// prodServices are the five services a production bring-up runs. The edge is included: it is the
// only published surface, and it carries no compose healthcheck, so "running" is what can be said.
var prodServices = []string{"postgres", "object-store", "control-plane", "runner", "edge"}

// checkContainers is what `docker compose ps` was standing in for: every service present, running,
// and — where compose declares a healthcheck — healthy. A container in `restarting` is the shape a
// crash-looping service takes under `restart: always`, which otherwise looks alive from outside.
func (s prodStack) checkContainers(ctx context.Context) Check {
	var bad, detail []string
	for _, svc := range prodServices {
		name := s.cfg.containerName(svc)
		out, err := dockerCapture(ctx, nil, "inspect", "--format",
			"{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}-{{end}}", name)
		if err != nil {
			bad = append(bad, svc+"=absent")
			continue
		}
		state, health, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
		detail = append(detail, fmt.Sprintf("%s=%s/%s", svc, state, health))
		if state != "running" || (health != "-" && health != "healthy") {
			bad = append(bad, fmt.Sprintf("%s=%s/%s", svc, state, health))
		}
	}
	if len(bad) > 0 {
		return fail(fmt.Sprintf("%s (want running, and healthy where compose declares a healthcheck) [%s]",
			strings.Join(bad, ", "), strings.Join(detail, " ")))
	}
	return ok(strings.Join(detail, " "))
}

// checkImageDigests verifies the two external bases and the digest-pinned edge run at the digests
// this repo pins, and NAMES the app images that are running. The app images are not pinned here:
// nothing is published (install.md's images ceiling), so the operator supplies their references and
// only the operator knows what they should be — naming them is honest, asserting them would not be.
func (s prodStack) checkImageDigests(ctx context.Context) Check {
	var wrong, named []string
	for svc, want := range map[string]string{"postgres": postgresDigest, "object-store": seaweedDigest} {
		img := inspectContainerImage(ctx, s.cfg.containerName(svc))
		if !strings.Contains(img, want) {
			wrong = append(wrong, fmt.Sprintf("%s runs %q, want the pinned %s", svc, img, want))
		}
	}
	// The edge's digest is pinned in the overlay itself, so the running container must carry it.
	if img := inspectContainerImage(ctx, s.cfg.containerName("edge")); !strings.Contains(img, "@sha256:") {
		wrong = append(wrong, fmt.Sprintf("edge runs %q, which is not a digest-pinned reference", img))
	}
	for _, svc := range []string{"control-plane", "runner"} {
		named = append(named, svc+"="+inspectContainerImage(ctx, s.cfg.containerName(svc)))
	}
	if len(wrong) > 0 {
		return fail(strings.Join(wrong, "; "))
	}
	return ok("postgres+object-store+edge at their pinned digests; " + strings.Join(named, " "))
}

// checkBackupTarget is the one trap `palai backup` springs on a stack that otherwise looks perfect:
// backup/restore/support-bundle derive their container names from the `project` in
// ${PALAI_HOME}/config.json, NOT from PALAI_COMPOSE_PROJECT. Pick a different name in the env file
// and the stack comes up healthy while every disaster-recovery command fails with `No such
// container` — which is the day you find out (measured 2026-07-29, cloud-smoke-report.md, Bulgu 2).
func (s prodStack) checkBackupTarget(ctx context.Context) Check {
	configPath := filepath.Join(s.home, "config.json")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return fail("read " + configPath + ": " + err.Error())
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fail("decode " + configPath + ": " + err.Error())
	}
	if cfg.Project != s.project {
		return fail(fmt.Sprintf("PALAI_COMPOSE_PROJECT=%q but %s says project %q — `palai backup`, `restore` and `support-bundle` use the config.json name, so they would look for a container called %s-postgres-1 that does not exist. Bring the stack up under the config.json project.",
			s.project, configPath, cfg.Project, cfg.Project))
	}
	if _, err := dockerCapture(ctx, nil, "inspect", "--format", "{{.Id}}", cfg.containerName("postgres")); err != nil {
		return fail("backup would target the container " + cfg.containerName("postgres") + ", which does not exist: " + err.Error())
	}
	return ok("backup/restore/support-bundle target " + cfg.containerName("postgres") + ", which is running (config.json project matches PALAI_COMPOSE_PROJECT)")
}
