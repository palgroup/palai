//go:build component

package fleet_test

// The Fleet PRESENCE proofs against a REAL Postgres (device plan DoD 10, T6).
//
// ‼️ THEY EXIST BECAUSE THE FEATURES THEY GUARD SHIPPED ON A HAND-TAKEN MEASUREMENT. Both were verified
// live — an enrolled Mac reporting its build and its isolation modes, and connection_state flipping
// online/offline as the agent was started and killed — and that is exactly the shape this tree keeps
// recording as insufficient: one marker, one machine, one pass, and nothing that re-runs it. A guard is
// what makes the property survive the next edit.
//
// The package is run WHOLE by scripts/test/component's postgres suite (no -run filter), so a test added
// here runs; device_identity_component_test.go states that check and it was re-read rather than assumed.

import (
	"context"
	"testing"

	api2 "github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/fleet"
	"github.com/palgroup/palai/packages/device"
)

// enrolled registers one machine the way an installed agent does and returns its id. It uses the same
// deviceEnrolment fixture the identity proofs drive, so a change to what an enrolment carries reaches
// these tests rather than passing them by.
func enrolled(t *testing.T, registry *fleet.Store, poolID, label string, modes []string) string {
	t.Helper()
	reg := deviceEnrolment(poolID, label, newID("fp"))
	if modes != nil {
		reg.IsolationModes = modes
	}
	row, err := registry.Register(context.Background(), reg)
	if err != nil {
		t.Fatalf("enrol %s: %v", label, err)
	}
	return row.ID
}

// stubPresence is a live gateway that answers whatever a case sets. It is a stub rather than a real
// gateway because the property under proof is the PROJECTION — that what the gateway says reaches the
// panel, and that a caller which asked nothing renders nothing — and a real gateway would make the test
// about websockets instead.
type stubPresence struct {
	connections map[string]int64
	leases      map[string]int64
}

func (s stubPresence) RunnerActiveLeases(id string) int64 { return s.leases[id] }
func (s stubPresence) RunnerConnections(id string) int64  { return s.connections[id] }
func (s stubPresence) CordonRunner(string)                {}
func (s stubPresence) ResumeRunner(string)                {}
func (s stubPresence) RevokeRunner(string)                {}
func (s stubPresence) ApproveRunner(string)               {}

// Waiting completes the interface. Zero because these cases are about ONE machine's presence, not about
// a pool's queue depth — and a stub that invented a number here would put an unrelated fact on the
// projection under test.
func (s stubPresence) Waiting(string) int { return 0 }

func TestAConnectedMachineReadsOnlineAndADisconnectedOneReadsOffline(t *testing.T) {
	pool := openDeviceSpine(t)
	project, poolID := tenantFixture(t, pool, "unsandboxed-host")
	registry := fleet.NewStore(pool, newID, nil)
	id := enrolled(t, registry, poolID, "presence-online", nil)

	online := fleet.NewRegistryAPI(registry, nil).WithLifecycle(stubPresence{connections: map[string]int64{id: 1}})
	item, found, err := online.GetRunner(context.Background(), project, id)
	if err != nil || !found {
		t.Fatalf("read the runner: found=%v err=%v", found, err)
	}
	if item.Connections == nil || *item.Connections != 1 {
		t.Fatalf("a machine holding one session reports connections=%v, want 1", item.Connections)
	}

	offline := fleet.NewRegistryAPI(registry, nil).WithLifecycle(stubPresence{})
	item, _, err = offline.GetRunner(context.Background(), project, id)
	if err != nil {
		t.Fatalf("read the runner: %v", err)
	}
	if item.Connections == nil || *item.Connections != 0 {
		t.Fatalf("a machine holding no session reports connections=%v, want 0 — and 0 is an ANSWER, not an absence", item.Connections)
	}
}

func TestTwoSessionsAreOneOnlineMachine(t *testing.T) {
	pool := openDeviceSpine(t)
	project, poolID := tenantFixture(t, pool, "unsandboxed-host")
	registry := fleet.NewStore(pool, newID, nil)
	id := enrolled(t, registry, poolID, "presence-two-loops", nil)

	api := fleet.NewRegistryAPI(registry, nil).WithLifecycle(stubPresence{connections: map[string]int64{id: 2}})
	items, err := api.ListRunners(context.Background(), project, api2.RunnerListWindow{Limit: 50})
	if err != nil {
		t.Fatalf("list runners: %v", err)
	}
	seen := 0
	for _, item := range items {
		if item.ID != id {
			continue
		}
		seen++
		if item.Connections == nil || *item.Connections != 2 {
			t.Fatalf("connections=%v, want 2", item.Connections)
		}
	}
	if seen != 1 {
		t.Fatalf("the machine appears %d times in the listing, want exactly 1: two parked loops are one machine", seen)
	}
}

func TestAReadThatConsultedNoGatewayRendersNoPresence(t *testing.T) {
	pool := openDeviceSpine(t)
	project, poolID := tenantFixture(t, pool, "unsandboxed-host")
	registry := fleet.NewStore(pool, newID, nil)
	id := enrolled(t, registry, poolID, "presence-no-gateway", nil)

	item, found, err := fleet.NewRegistryAPI(registry, nil).GetRunner(context.Background(), project, id)
	if err != nil || !found {
		t.Fatalf("read the runner: found=%v err=%v", found, err)
	}
	if item.Connections != nil {
		t.Fatalf("a read with no gateway reported connections=%d; \"nobody asked\" and \"nothing connected\" must not look the same", *item.Connections)
	}
}

func TestTheMachinesOwnReportReachesTheProjection(t *testing.T) {
	pool := openDeviceSpine(t)
	project, poolID := tenantFixture(t, pool, "unsandboxed-host")
	registry := fleet.NewStore(pool, newID, nil)
	id := enrolled(t, registry, poolID, "presence-report", []string{device.IsolationContainer, device.IsolationUser})

	item, found, err := fleet.NewRegistryAPI(registry, nil).GetRunner(context.Background(), project, id)
	if err != nil || !found {
		t.Fatalf("read the runner: found=%v err=%v", found, err)
	}
	if item.AgentVersion != "9.9.9" {
		t.Fatalf("agent_version = %q, want the version the machine reported — the panel cannot say what a fleet runs from a column nothing selects", item.AgentVersion)
	}
	if item.IsolationModes != "container,user" {
		t.Fatalf("isolation_modes = %q, want \"container,user\": a machine that claimed no `accounts` must not read as one that can", item.IsolationModes)
	}
}
