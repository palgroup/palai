package workers

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Gateway is the OUTBOUND-ENROLLED transport surface for capability workers — the runner precedent (§31, §3
// seam): a worker DIALS this surface with a one-time enrollment token, receives a short-lived workload
// identity, then polls for jobs, redeems handles, and submits results. NO inbound port is EVER opened to the
// worker — the Gateway is the only listener; it dials nothing back. The surface exposes EXACTLY four typed
// operations (enroll/claim/redeem/result); there is no generic connect/proxy/exec route, so an ordinary
// sandbox worker cannot be used as a general tunnel (§31.5). Any other path or method is 404/405.
//
// The Gateway holds the AUTHORITATIVE claim server-side (keyed by the worker session + job): a worker sends
// only a job id, never a fence or a deadline, so it cannot forge either. It also holds the fixture artifact
// map (in-memory) so a typed job's input artifact reaches the worker and its output artifact round-trips
// back. ponytail: in-memory enrollment-token / workload-session / artifact maps — the fixture/reference
// scale; the durable object store (E09) and a renewal ceremony (the runner renewal seam) are the reuse path
// when a real fleet enrolls.
type Gateway struct {
	store *Store
	ttl   time.Duration

	mu          sync.Mutex
	enrollment  map[string]enrollGrant   // one-time enrollment token -> its tenant/capability grant
	sessions    map[string]*workerSess   // workload token -> live worker session
	artifacts   map[string][]byte        // artifact ref -> bytes (fixture object store)
	artifactSeq int64
}

type enrollGrant struct {
	tenant     Tenant
	capability string
}

type workerSess struct {
	workerID string
	tenant   Tenant
	notAfter time.Time
	claims   map[string]Claim    // job id -> the authoritative claim the gateway issued
	redeemed map[string][][]byte // job id -> secret VALUES redeemed for it, so a result echoing one is refused
}

// NewGateway builds the surface over a store. ttl is the short-lived workload-identity lifetime (a claim/
// redeem/result under an expired identity is 401 — the runner short-lived-cert precedent).
func NewGateway(store *Store, ttl time.Duration) *Gateway {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &Gateway{
		store:      store,
		ttl:        ttl,
		enrollment: map[string]enrollGrant{},
		sessions:   map[string]*workerSess{},
		artifacts:  map[string][]byte{},
	}
}

// IssueEnrollmentToken mints a ONE-TIME enrollment token bound to a tenant + capability. It is presented once
// on enroll and spent; it is never a durable credential (the runner one-use-token precedent). Provisioned
// out of band to the worker (the operator hands it over), exactly as a runner bootstrap token is.
func (g *Gateway) IssueEnrollmentToken(tenant Tenant, capability string) string {
	tok := randToken("cwenr")
	g.mu.Lock()
	g.enrollment[tok] = enrollGrant{tenant: tenant, capability: capability}
	g.mu.Unlock()
	return tok
}

// PutInputArtifact stores an input artifact for a job and returns its ref (the fixture object store).
func (g *Gateway) PutInputArtifact(data []byte) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.artifactSeq++
	ref := "cwart_in_" + itoaGW(g.artifactSeq)
	g.artifacts[ref] = append([]byte(nil), data...)
	return ref
}

// OutputArtifact returns an artifact's bytes by ref (for a test to verify the round-trip).
func (g *Gateway) OutputArtifact(ref string) ([]byte, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	b, ok := g.artifacts[ref]
	return b, ok
}

// Handler mounts the four typed operations. Everything else is 404 — there is no tunnel.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/capability/enroll", g.handleEnroll)
	mux.HandleFunc("/capability/claim", g.handleClaim)
	mux.HandleFunc("/capability/redeem", g.handleRedeem)
	mux.HandleFunc("/capability/result", g.handleResult)
	return mux
}

// --- enroll -----------------------------------------------------------------

type enrollReq struct {
	CapabilityVersion string            `json:"capability_version"`
	OS                string            `json:"os"`
	Arch              string            `json:"arch"`
	ToolchainDigests  map[string]string `json:"toolchain_digests"`
	Capacity          int               `json:"capacity"`
	PoolLabel         string            `json:"pool_label"`
}

type enrollResp struct {
	WorkerID      string `json:"worker_id"`
	WorkloadToken string `json:"workload_token"`
	NotAfter      string `json:"not_after"`
}

func (g *Gateway) handleEnroll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := bearer(r)
	g.mu.Lock()
	grant, ok := g.enrollment[token]
	if ok {
		delete(g.enrollment, token) // one-time: spent on first use
	}
	g.mu.Unlock()
	if !ok {
		http.Error(w, "invalid or spent enrollment token", http.StatusUnauthorized)
		return
	}
	var req enrollReq
	if err := readJSON(r, &req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	worker, err := g.store.Enroll(r.Context(), grant.tenant, WorkerSpec{
		Capability: grant.capability, CapabilityVersion: req.CapabilityVersion, OS: req.OS, Arch: req.Arch,
		ToolchainDigests: req.ToolchainDigests, Capacity: req.Capacity, PoolLabel: req.PoolLabel, TrustLabel: "sandbox",
	})
	if err != nil {
		http.Error(w, "enrollment refused", http.StatusForbidden)
		return
	}
	workload := randToken("cwwl")
	notAfter := time.Now().Add(g.ttl)
	g.mu.Lock()
	g.sessions[workload] = &workerSess{workerID: worker.ID, tenant: grant.tenant, notAfter: notAfter, claims: map[string]Claim{}, redeemed: map[string][][]byte{}}
	g.mu.Unlock()
	writeJSON(w, http.StatusOK, enrollResp{WorkerID: worker.ID, WorkloadToken: workload, NotAfter: notAfter.UTC().Format(time.RFC3339)})
}

// --- claim ------------------------------------------------------------------

type claimResp struct {
	JobID            string   `json:"job_id"`
	Operation        string   `json:"operation"`
	InputArtifact    string   `json:"input_artifact_b64"` // the fixture carries the input bytes inline
	SecretHandleRefs []string `json:"secret_handle_refs"`
	DeadlineUnix     int64    `json:"deadline_unix"`
}

func (g *Gateway) handleClaim(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sess, ok := g.session(r)
	if !ok {
		http.Error(w, "invalid or expired workload identity", http.StatusUnauthorized)
		return
	}
	claim, got, err := g.store.ClaimNext(r.Context(), sess.tenant, sess.workerID)
	if err != nil {
		http.Error(w, "claim failed", http.StatusInternalServerError)
		return
	}
	if !got {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	g.mu.Lock()
	sess.claims[claim.JobID] = claim
	var input []byte
	if len(claim.InputRefs) > 0 {
		input = g.artifacts[claim.InputRefs[0]]
	}
	g.mu.Unlock()
	resp := claimResp{JobID: claim.JobID, Operation: claim.Operation, SecretHandleRefs: claim.SecretHandleRefs}
	if len(input) > 0 {
		resp.InputArtifact = base64.StdEncoding.EncodeToString(input)
	}
	if !claim.Deadline.IsZero() {
		resp.DeadlineUnix = claim.Deadline.Unix()
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- redeem -----------------------------------------------------------------

type redeemReq struct {
	JobID      string `json:"job_id"`
	HandleName string `json:"handle_name"`
}

type redeemResp struct {
	ValueB64 string `json:"value_b64"`
}

func (g *Gateway) handleRedeem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sess, ok := g.session(r)
	if !ok {
		http.Error(w, "invalid or expired workload identity", http.StatusUnauthorized)
		return
	}
	var req redeemReq
	if err := readJSON(r, &req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	claim, held := sess.claims[req.JobID]
	g.mu.Unlock()
	if !held {
		http.Error(w, "no such claim for this worker", http.StatusForbidden)
		return
	}
	value, err := g.store.RedeemSecretHandle(r.Context(), claim, req.HandleName)
	if err != nil {
		// The scope/expiry/fence reasons are all a redeem refusal to the worker; do not echo which.
		http.Error(w, "redeem refused", http.StatusForbidden)
		return
	}
	// Remember what we handed out for this job so handleResult can REFUSE to journal it back — WRK-004 must
	// hold against ANY worker, not just the honest fixture.
	if len(value) > 0 {
		g.mu.Lock()
		sess.redeemed[req.JobID] = append(sess.redeemed[req.JobID], append([]byte(nil), value...))
		g.mu.Unlock()
	}
	writeJSON(w, http.StatusOK, redeemResp{ValueB64: base64.StdEncoding.EncodeToString(value)})
}

// --- result -----------------------------------------------------------------

type resultReq struct {
	JobID          string         `json:"job_id"`
	Class          string         `json:"class"`
	Operation      string         `json:"operation"`
	Receipt        map[string]any `json:"receipt"`
	OutputArtifact string         `json:"output_artifact_b64"`
}

func (g *Gateway) handleResult(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sess, ok := g.session(r)
	if !ok {
		http.Error(w, "invalid or expired workload identity", http.StatusUnauthorized)
		return
	}
	var req resultReq
	if err := readJSON(r, &req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	claim, held := sess.claims[req.JobID]
	g.mu.Unlock()
	if !held {
		http.Error(w, "no such claim for this worker", http.StatusForbidden)
		return
	}
	var outputData []byte
	if req.OutputArtifact != "" {
		if data, err := base64.StdEncoding.DecodeString(req.OutputArtifact); err == nil {
			outputData = data
		}
	}
	// SHOULD-FIX 3: a redeemed secret VALUE must never reach the durable, evidence-visible journal. The store
	// writes the worker-controlled receipt/output VERBATIM, so REFUSE here — before any persistence — a
	// result that echoes a value this session redeemed for this job. This is what makes WRK-004 hold against
	// a hostile/buggy worker, not only the honest fixture.
	g.mu.Lock()
	redeemed := sess.redeemed[req.JobID]
	g.mu.Unlock()
	if len(redeemed) > 0 {
		receiptJSON, _ := json.Marshal(req.Receipt)
		if containsAnySecret(redeemed, receiptJSON, outputData) {
			http.Error(w, "result rejected: would leak a redeemed secret", http.StatusForbidden)
			return
		}
	}
	var outputRefs []string
	if len(outputData) > 0 {
		g.mu.Lock()
		g.artifactSeq++
		ref := "cwart_out_" + itoaGW(g.artifactSeq)
		g.artifacts[ref] = outputData
		g.mu.Unlock()
		outputRefs = []string{ref}
	}
	err := g.store.SubmitResult(r.Context(), claim, Outcome{
		Class: req.Class, Operation: req.Operation, Receipt: req.Receipt, OutputRefs: outputRefs,
	})
	switch {
	case err == nil:
		g.mu.Lock()
		delete(sess.claims, req.JobID)
		delete(sess.redeemed, req.JobID)
		g.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"accepted": true, "output_refs": outputRefs})
	case errors.Is(err, ErrStaleFence), errors.Is(err, ErrWorkerFenced), errors.Is(err, ErrNotLeaseholder):
		http.Error(w, "stale fence", http.StatusConflict)
	case errors.Is(err, ErrUntypedOperation):
		http.Error(w, "untyped operation (no tunnel)", http.StatusForbidden)
	default:
		http.Error(w, "result rejected", http.StatusBadRequest)
	}
}

// --- helpers ----------------------------------------------------------------

func (g *Gateway) session(r *http.Request) (*workerSess, bool) {
	token := bearer(r)
	g.mu.Lock()
	defer g.mu.Unlock()
	sess, ok := g.sessions[token]
	if !ok || time.Now().After(sess.notAfter) {
		return nil, false
	}
	return sess, true
}

// containsAnySecret reports whether any non-empty secret value appears verbatim in any haystack (the
// serialized receipt and the decoded output artifact) — the server-side guard that a redeemed value never
// lands in the journal, regardless of what the worker put in its result.
func containsAnySecret(secrets [][]byte, haystacks ...[]byte) bool {
	for _, sec := range secrets {
		if len(sec) == 0 {
			continue
		}
		for _, h := range haystacks {
			if len(h) > 0 && bytes.Contains(h, sec) {
				return true
			}
		}
	}
	return false
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8<<20)).Decode(v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func randToken(prefix string) string {
	var raw [24]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(raw[:])
}

func itoaGW(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
