//go:build component

package extensions

import (
	"context"
	"testing"
)

// TestHookCRUDRoundtrip proves a created hook reads back byte-stable through the store (spec §28.17): the
// admin create → GetHook roundtrip preserves point/category/executor/config/secret_ref, a disable flips the
// kill-switch (and a re-disable is a no-op reporting the hook still exists), and a duplicate name in the same
// project is a typed collision.
func TestHookCRUDRoundtrip(t *testing.T) {
	s, org, project := openStore(t)
	ctx := context.Background()

	body := `{"name":"guard","hook_point":"before_tool","category":"policy","executor":"remote_http","config":{"url":"https://hooks.example/deny"},"secret_ref":"sref_hook"}`
	created, err := s.CreateHook(ctx, org, project, []byte(body))
	if err != nil {
		t.Fatalf("CreateHook() error = %v", err)
	}
	got, err := s.GetHook(ctx, org, project, created.ID)
	if err != nil {
		t.Fatalf("GetHook() error = %v", err)
	}
	if got.Name != "guard" || got.HookPoint != "before_tool" || got.Category != "policy" || got.Executor != "remote_http" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if got.SecretRef != "sref_hook" || got.Config["url"] != "https://hooks.example/deny" {
		t.Fatalf("roundtrip lost secret_ref/config: %+v", got)
	}
	if got.Disabled {
		t.Fatal("fresh hook reads back disabled")
	}

	existed, err := s.DisableHook(ctx, org, project, created.ID)
	if err != nil || !existed {
		t.Fatalf("DisableHook() = (%v, %v), want (true, nil)", existed, err)
	}
	// A re-disable is a no-op but still reports the hook exists (disambiguated from unknown).
	if existed, err := s.DisableHook(ctx, org, project, created.ID); err != nil || !existed {
		t.Fatalf("re-DisableHook() = (%v, %v), want (true, nil)", existed, err)
	}
	if reread, err := s.GetHook(ctx, org, project, created.ID); err != nil || !reread.Disabled {
		t.Fatalf("after disable GetHook disabled = %v (err %v), want true", reread.Disabled, err)
	}
	// An unknown id reports not-existing (not an error).
	if existed, err := s.DisableHook(ctx, org, project, "hook_missing"); err != nil || existed {
		t.Fatalf("DisableHook(unknown) = (%v, %v), want (false, nil)", existed, err)
	}

	// A duplicate name in the same project is a typed collision.
	if _, err := s.CreateHook(ctx, org, project, []byte(body)); err == nil {
		t.Fatal("duplicate hook name accepted, want ErrHookNameCollision")
	}
}

// TestHookOrderDeterministicRegistrationOrder proves the dispatch load walks a point's hooks in registration
// order (created_at, id) — the documented deterministic firing sequence (spec §28.17). Three hooks registered
// in sequence at before_tool come back in that exact order; a disabled hook is skipped; a hook at a different
// point is not returned.
func TestHookOrderDeterministicRegistrationOrder(t *testing.T) {
	s, org, project := openStore(t)
	ctx := context.Background()

	mk := func(name string) string {
		h, err := s.CreateHook(ctx, org, project,
			[]byte(`{"name":"`+name+`","hook_point":"before_tool","category":"observer","executor":"platform_inline","config":{"handler":"note"}}`))
		if err != nil {
			t.Fatalf("CreateHook(%s) error = %v", name, err)
		}
		return h.ID
	}
	first := mk("h1")
	second := mk("h2")
	third := mk("h3")

	// A hook at a DIFFERENT point must not appear in the before_tool load.
	if _, err := s.CreateHook(ctx, org, project,
		[]byte(`{"name":"other","hook_point":"on_terminal","category":"observer","executor":"platform_inline","config":{"handler":"note"}}`)); err != nil {
		t.Fatalf("CreateHook(other point) error = %v", err)
	}

	loaded, err := s.loadHooks(ctx, org, project, "before_tool")
	if err != nil {
		t.Fatalf("loadHooks() error = %v", err)
	}
	if len(loaded) != 3 {
		t.Fatalf("loaded %d hooks, want 3 (only the before_tool ones)", len(loaded))
	}
	if loaded[0].ID != first || loaded[1].ID != second || loaded[2].ID != third {
		t.Fatalf("hooks out of registration order: got [%s %s %s], want [%s %s %s]",
			loaded[0].ID, loaded[1].ID, loaded[2].ID, first, second, third)
	}

	// A disabled hook is skipped by the dispatch load.
	if _, err := s.DisableHook(ctx, org, project, second); err != nil {
		t.Fatalf("DisableHook() error = %v", err)
	}
	loaded, err = s.loadHooks(ctx, org, project, "before_tool")
	if err != nil {
		t.Fatalf("loadHooks() after disable error = %v", err)
	}
	if len(loaded) != 2 || loaded[0].ID != first || loaded[1].ID != third {
		t.Fatalf("after disabling h2, load = %v, want [%s %s]", loaded, first, third)
	}
}
