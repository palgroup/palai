package execution

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
)

// countingStore is a SnapshotObjectStore that records whether Put ran — so the overflow test can prove
// an oversize archive is rejected BEFORE any object is written (no orphan).
type countingStore struct{ puts int }

func (c *countingStore) Put(context.Context, string, []byte) (string, int64, error) {
	c.puts++
	return "sha256:x", 0, nil
}
func (c *countingStore) Get(context.Context, string) ([]byte, bool, error) { return nil, false, nil }

// TestSnapshotCaptureRejectsOversizeBeforePut proves the size bound rejects an archive over
// MaxSnapshotArchiveBytes BEFORE the object-store Put (spec §29.10) — so an oversize snapshot leaves no
// orphan object. The bound is lowered so a tiny fixture triggers it; the overflow returns before the
// spine is touched, so a nil spine is safe here.
func TestSnapshotCaptureRejectsOversizeBeforePut(t *testing.T) {
	orig := MaxSnapshotArchiveBytes
	MaxSnapshotArchiveBytes = 16 // any real archive exceeds this
	defer func() { MaxSnapshotArchiveBytes = orig }()

	dir := t.TempDir()
	if err := workspace.Prepare(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, workspace.ScratchDir, "big.txt"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}

	store := &countingStore{}
	sink := NewSnapshotSink(store, nil) // nil spine: the overflow returns before any spine call
	_, err := sink.Capture(context.Background(), SnapshotCaptureInput{
		SnapshotID: "snap_x", Organization: "o", Project: "p", WorkspaceID: "w", AllocationID: "a", HostPath: dir,
	})
	if err == nil {
		t.Fatal("Capture(oversize) returned nil, want the size-bound rejection")
	}
	if store.puts != 0 {
		t.Fatalf("oversize archive was PUT %d time(s) — it must be rejected before any object write", store.puts)
	}
}
