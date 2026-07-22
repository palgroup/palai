package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// fakeDocker is an in-test container lister/remover that HONOURS the label filter (as the real daemon does)
// and records what the sweep asked it to remove. It proves the sweep's daemon-side scoping + grace logic.
type fakeDocker struct {
	items     []container.Summary
	removed   []string
	lastLabel string
}

func (f *fakeDocker) ContainerList(_ context.Context, opts client.ContainerListOptions) (client.ContainerListResult, error) {
	// Record and honour the label filter, exactly like the daemon: only matching-label items are returned.
	for label := range opts.Filters["label"] {
		f.lastLabel = label
	}
	var matched []container.Summary
	for _, it := range f.items {
		if f.lastLabel == sandboxLabel+"="+it.Labels[sandboxLabel] {
			matched = append(matched, it)
		}
	}
	return client.ContainerListResult{Items: matched}, nil
}

func (f *fakeDocker) ContainerRemove(_ context.Context, id string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.removed = append(f.removed, id)
	return client.ContainerRemoveResult{}, nil
}

// TestSweepScopesToMCPLabelAndGrace proves the sweep (1) asks the daemon ONLY for the mcp label (so an
// engine container is never a candidate), (2) reclaims an aged mcp container, and (3) spares a fresh one
// inside the grace window.
func TestSweepScopesToMCPLabelAndGrace(t *testing.T) {
	now := time.Unix(10_000, 0)
	fake := &fakeDocker{items: []container.Summary{
		{ID: "aged_mcp", Created: now.Add(-10 * time.Minute).Unix(), Labels: map[string]string{sandboxLabel: sandboxLabelMCP}},
		{ID: "fresh_mcp", Created: now.Add(-10 * time.Second).Unix(), Labels: map[string]string{sandboxLabel: sandboxLabelMCP}},
		{ID: "engine", Created: now.Add(-1 * time.Hour).Unix(), Labels: map[string]string{sandboxLabel: "engine"}},
	}}
	s := NewSweeperWithClient(fake, 5*time.Minute)
	s.now = func() time.Time { return now }

	reclaimed, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if reclaimed != 1 {
		t.Fatalf("reclaimed = %d, want 1 (only the aged mcp container)", reclaimed)
	}
	if len(fake.removed) != 1 || fake.removed[0] != "aged_mcp" {
		t.Fatalf("removed = %v, want only aged_mcp (fresh spared, engine never a candidate)", fake.removed)
	}
	if fake.lastLabel != sandboxLabel+"="+sandboxLabelMCP {
		t.Fatalf("daemon filter label = %q, want %s=%s (engine label never listed)", fake.lastLabel, sandboxLabel, sandboxLabelMCP)
	}
}
