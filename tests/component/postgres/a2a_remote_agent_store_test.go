//go:build component

package postgres

import (
	"context"
	"testing"

	"github.com/palgroup/palai/adapters/integrations/a2a"
)

// TestA2ARemoteAgentStoreRLS proves the migration-000039 client-registration store against real PostgreSQL:
//  1. a registered remote agent is readable only within its owning tenant (RLS) — a foreign tenant sees nothing;
//  2. the trust-envelope pins (endpoint, negotiated version, allowlists, timeout) round-trip intact;
//  3. auth_connection_ref is stored as the secret_ref HANDLE it was given — a HANDLE, never a bearer value
//     (A2A-005/SUB-007: the credential is redeemed at call time, never persisted as a secret here).
func TestA2ARemoteAgentStoreRLS(t *testing.T) {
	cs := openHarness(t)
	pool := cs.Pool()
	tenantA, _, _ := seedRun(t, pool)
	tenantB, _, _ := seedRun(t, pool)

	store := a2a.NewStore(pool, newID)
	ctx := context.Background()

	agent := a2a.RemoteAgent{
		Organization: tenantA.Organization, Project: tenantA.Project,
		Name:                 "Remote Planner",
		CardURL:              "https://peer.example/agent-card.json",
		Endpoint:             "https://peer.example/v1/a2a",
		ProtocolVersion:      "1.0",
		AuthConnectionRef:    "secref_conn_remote_1", // a HANDLE, not a bearer
		AllowedInputModes:    []string{"text/plain"},
		AllowedOutputModes:   []string{"text/plain"},
		AllowedExtensionURIs: []string{"https://peer.example/ext/ok"},
		DataPolicy:           "minimum",
		MaxCostCents:         500,
		TimeoutMS:            15000,
		MaxOutputBytes:       1 << 20,
	}
	id, err := store.RegisterRemoteAgent(ctx, agent)
	if err != nil {
		t.Fatalf("RegisterRemoteAgent: %v", err)
	}

	// (1) RLS: tenant A reads it; tenant B does not.
	got, ok, err := store.GetRemoteAgent(ctx, tenantA.Organization, tenantA.Project, id)
	if err != nil || !ok {
		t.Fatalf("owning tenant cannot read its remote agent: ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.GetRemoteAgent(ctx, tenantB.Organization, tenantB.Project, id); err != nil || ok {
		t.Fatalf("foreign tenant read a cross-tenant remote agent (RLS breach): ok=%v err=%v", ok, err)
	}

	// (2) Trust-envelope pins round-trip.
	if got.Endpoint != agent.Endpoint || got.ProtocolVersion != "1.0" || got.TimeoutMS != 15000 || got.MaxCostCents != 500 {
		t.Fatalf("trust-envelope pins did not round-trip: %+v", got)
	}
	if len(got.AllowedExtensionURIs) != 1 || got.AllowedExtensionURIs[0] != "https://peer.example/ext/ok" {
		t.Fatalf("extension allowlist did not round-trip: %+v", got.AllowedExtensionURIs)
	}

	// (3) auth_connection_ref is the HANDLE it was given (never a bearer value).
	if got.AuthConnectionRef != "secref_conn_remote_1" {
		t.Fatalf("auth_connection_ref = %q, want the stored secret_ref handle", got.AuthConnectionRef)
	}
}
