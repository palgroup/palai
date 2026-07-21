//go:build e2e

package responses

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/coordinator/recovery"
)

// memCheckpointStore is an in-memory CheckpointObjectStore (Put + Get) for the recovery-ladder e2e:
// it holds the opaque checkpoint bytes keyed by object key, counts Get calls so a test can prove the
// exact rung never reads a checkpoint, and can tamper a stored object to fail the §26.4 integrity
// condition. Its checksum matches artifacts.Store's "sha256:<hex>" so a clean restore verifies.
type memCheckpointStore struct {
	mu   sync.Mutex
	objs map[string][]byte
	gets int
}

func newMemCheckpointStore() *memCheckpointStore {
	return &memCheckpointStore{objs: map[string][]byte{}}
}

func (s *memCheckpointStore) Put(_ context.Context, key string, body []byte) (string, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objs[key] = append([]byte(nil), body...)
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), int64(len(body)), nil
}

func (s *memCheckpointStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	b, ok := s.objs[key]
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), b...), true, nil
}

func (s *memCheckpointStore) getCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets
}

// tamperAll corrupts every stored object's bytes so the restore's sha256 no longer matches the
// recorded checksum — the §26.4 integrity condition fails and the checkpoint is rejected (ENG-010).
func (s *memCheckpointStore) tamperAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, b := range s.objs {
		if len(b) > 0 {
			b[0] ^= 0xff
			s.objs[k] = b
		}
	}
}

func (s *memCheckpointStore) objectCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.objs)
}

// countingDialer wraps a dialer and counts Dial calls, so the exact rung can be proven to stand down
// WITHOUT dialing a fresh engine (ENG-008).
type countingDialer struct {
	inner execution.EngineDialer
	mu    sync.Mutex
	count int
}

func (d *countingDialer) Dial(ctx context.Context, a execution.AttemptDescriptor) (execution.EngineChannel, error) {
	d.mu.Lock()
	d.count++
	d.mu.Unlock()
	return d.inner.Dial(ctx, a)
}

func (d *countingDialer) dials() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.count
}

// checkpointSink builds a CheckpointSink over a caller store + the real recovery persistence layer,
// so a recovery e2e persists checkpoints to a swappable in-memory object store.
func (h *harness) checkpointSink(store execution.CheckpointObjectStore) *execution.CheckpointSink {
	return execution.NewCheckpointSink(store, recovery.New(h.spine.Pool()))
}

// recoveryEventLevels reads the level field of every attempt.recovering.v1 journaled for a session.
func (h *harness) recoveryEventLevels(sessionID string) []string {
	h.t.Helper()
	return h.eventLevels(sessionID, "attempt.recovering.v1")
}

func (h *harness) eventLevels(sessionID, typ string) []string {
	h.t.Helper()
	rows, err := h.spine.Pool().Query(context.Background(),
		`SELECT payload FROM events WHERE session_id=$1 AND organization_id=$2 AND project_id=$3 AND type=$4 ORDER BY seq`,
		sessionID, h.tenant.Organization, h.tenant.Project, typ)
	if err != nil {
		h.t.Fatalf("read %s events error = %v", typ, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			h.t.Fatalf("scan %s payload error = %v", typ, err)
		}
		var body struct {
			Level string `json:"level"`
		}
		_ = json.Unmarshal(payload, &body)
		out = append(out, body.Level)
	}
	return out
}

// recoveryProof reads the single §26.12 RecoveryProof journaled for a session (recovery.proof.v1).
func (h *harness) recoveryProof(sessionID string) (recovery.RecoveryProof, bool) {
	h.t.Helper()
	var payload []byte
	err := h.spine.Pool().QueryRow(context.Background(),
		`SELECT payload FROM events WHERE session_id=$1 AND organization_id=$2 AND project_id=$3 AND type='recovery.proof.v1' ORDER BY seq DESC LIMIT 1`,
		sessionID, h.tenant.Organization, h.tenant.Project).Scan(&payload)
	if err != nil {
		return recovery.RecoveryProof{}, false
	}
	var proof recovery.RecoveryProof
	if err := json.Unmarshal(payload, &proof); err != nil {
		h.t.Fatalf("decode RecoveryProof error = %v", err)
	}
	return proof, true
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestLadderPrefersExactWhenLeaseAlive proves the exact rung (ENG-008, spec §26.3 rung 1): while the
// ORIGINAL attempt still holds a live response.run lease, a second attempt on the same run stands
// down WITHOUT dialing a fresh engine or reading the checkpoint — the original run finishes
// untouched, and the exact rung is journaled and visible.
func TestLadderPrefersExactWhenLeaseAlive(t *testing.T) {
	h := newHarness(t)
	gp := newGatedProvider()
	store := newMemCheckpointStore()
	dialer := &countingDialer{inner: subprocessDialer{engineDir: h.engineDir}}
	orch := h.newOrchestratorWithAdapter(dialer, gp)
	orch.SetCheckpointSink(h.checkpointSink(store))
	stop := h.runWorker(orch)
	defer stop()

	respID, sessionID, runID := h.admit()

	// attempt-1 parks mid-run: its response.run job is claimed with a live lease.
	select {
	case <-gp.started:
	case <-time.After(30 * time.Second):
		t.Fatal("first model step never started")
	}
	if d := dialer.dials(); d != 1 {
		t.Fatalf("dials after attempt-1 start = %d, want 1", d)
	}

	// A SECOND attempt on the SAME run, direct-driven with no claimed job of its own: the original
	// lease is alive, so it takes the exact rung and stands down. No dial, no checkpoint read.
	if err := orch.ExecuteAttempt(context.Background(), h.descriptor(runID, 2)); err != nil {
		t.Fatalf("second attempt (exact stand-down) error = %v", err)
	}
	if d := dialer.dials(); d != 1 {
		t.Fatalf("dials after exact stand-down = %d, want 1 (the exact rung must NOT dial)", d)
	}
	if g := store.getCount(); g != 0 {
		t.Fatalf("checkpoint Get calls during exact stand-down = %d, want 0 (the checkpoint is untouched)", g)
	}
	if !contains(h.recoveryEventLevels(sessionID), string(recovery.LevelExact)) {
		t.Fatalf("no attempt.recovering.v1 at level=exact; levels = %v", h.recoveryEventLevels(sessionID))
	}

	// Release attempt-1: the original run finishes untouched.
	close(gp.release)
	h.awaitResponseState(respID, "completed", 60*time.Second)
}
