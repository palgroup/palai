package execution

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"
)

// fakeAdvancer records the commands AdvanceRun issues and can simulate a run that has
// already moved past a state (ErrInvalidState), one that is already terminal
// (ErrRunTerminal), or a hard transition failure.
type fakeAdvancer struct {
	calls    []statemachines.RunCommand
	invalid  map[statemachines.RunCommand]bool
	terminal map[statemachines.RunCommand]bool
	hardErr  error
}

func (f *fakeAdvancer) ApplyRunTransition(_ context.Context, _ coordinator.Tenant, _ string, cmd statemachines.RunCommand) (coordinator.Transition, error) {
	f.calls = append(f.calls, cmd)
	if f.hardErr != nil {
		return coordinator.Transition{}, f.hardErr
	}
	if f.terminal[cmd] {
		// The real store wraps both, so an invalid-state check still matches.
		return coordinator.Transition{}, fmt.Errorf("%w: %w", coordinator.ErrRunTerminal, statemachines.ErrInvalidState)
	}
	if f.invalid[cmd] {
		return coordinator.Transition{}, statemachines.ErrInvalidState
	}
	return coordinator.Transition{}, nil
}

func runClaim() coordinator.Claim {
	return coordinator.Claim{Tenant: coordinator.Tenant{Organization: "org", Project: "prj"}, JobID: "job_1", Fence: 1}
}

func TestAdvanceRunDrivesFreshRunToRunning(t *testing.T) {
	adv := &fakeAdvancer{}
	result, err := AdvanceRun(adv)(context.Background(), runClaim(), []byte(`{"run_id":"run_x"}`))
	if err != nil {
		t.Fatalf("AdvanceRun() error = %v", err)
	}
	if result != "run:run_x:assigned" {
		t.Fatalf("result = %q, want run:run_x:assigned", result)
	}
	want := []statemachines.RunCommand{statemachines.RunCmdProvision, statemachines.RunCmdStart}
	if len(adv.calls) != len(want) || adv.calls[0] != want[0] || adv.calls[1] != want[1] {
		t.Fatalf("commands = %v, want %v", adv.calls, want)
	}
}

func TestAdvanceRunResumesIdempotentlyAfterPartialAssign(t *testing.T) {
	// The run is already provisioning (a previous attempt provisioned then died): the
	// provision command is now invalid and must be skipped, not treated as a failure.
	adv := &fakeAdvancer{invalid: map[statemachines.RunCommand]bool{statemachines.RunCmdProvision: true}}
	if _, err := AdvanceRun(adv)(context.Background(), runClaim(), []byte(`{"run_id":"run_x"}`)); err != nil {
		t.Fatalf("resume after provision error = %v", err)
	}

	// The run is already running (fully assigned): both commands are skipped and the
	// redelivered job still succeeds so it can be completed and leave the queue.
	adv = &fakeAdvancer{invalid: map[statemachines.RunCommand]bool{
		statemachines.RunCmdProvision: true, statemachines.RunCmdStart: true,
	}}
	result, err := AdvanceRun(adv)(context.Background(), runClaim(), []byte(`{"run_id":"run_x"}`))
	if err != nil {
		t.Fatalf("resume after full assign error = %v", err)
	}
	if result != "run:run_x:assigned" {
		t.Fatalf("result = %q, want run:run_x:assigned", result)
	}
}

func TestAdvanceRunStopsOnTerminalRun(t *testing.T) {
	// The run reached a terminal state (e.g. canceled before dispatch) by another
	// path. The assign job must not treat that as "already applied" and report the
	// run assigned, and must stop rather than keep issuing commands at a terminal run.
	adv := &fakeAdvancer{terminal: map[statemachines.RunCommand]bool{statemachines.RunCmdProvision: true}}
	result, err := AdvanceRun(adv)(context.Background(), runClaim(), []byte(`{"run_id":"run_x"}`))
	if err != nil {
		t.Fatalf("AdvanceRun() on terminal run error = %v, want nil", err)
	}
	if result == "run:run_x:assigned" {
		t.Fatal("terminal run reported as assigned; want a distinct terminal result")
	}
	if len(adv.calls) != 1 {
		t.Fatalf("commands = %v, want to stop after the terminal rejection", adv.calls)
	}
}

func TestAdvanceRunRejectsMalformedPayload(t *testing.T) {
	adv := &fakeAdvancer{}
	if _, err := AdvanceRun(adv)(context.Background(), runClaim(), []byte(`{"run_id":""}`)); err == nil {
		t.Fatal("AdvanceRun() with empty run_id error = nil, want error")
	}
	if _, err := AdvanceRun(adv)(context.Background(), runClaim(), []byte(`not json`)); err == nil {
		t.Fatal("AdvanceRun() with invalid JSON error = nil, want error")
	}
	if len(adv.calls) != 0 {
		t.Fatalf("malformed payload issued commands %v, want none", adv.calls)
	}
}

func TestAdvanceRunPropagatesHardTransitionError(t *testing.T) {
	adv := &fakeAdvancer{hardErr: errors.New("run not found in tenant scope")}
	if _, err := AdvanceRun(adv)(context.Background(), runClaim(), []byte(`{"run_id":"run_x"}`)); err == nil {
		t.Fatal("AdvanceRun() with hard error = nil, want propagated error")
	}
}
