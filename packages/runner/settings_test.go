package runner

import (
	"context"
	"sync"
	"testing"
	"time"
)

// THE DEFECT THESE TESTS EXIST FOR, stated as the measurement that found it (2026-08-03, live stack):
//
//	deployment_desired runner_pool/pool_default  written_at  2026-08-03 08:08:03+00
//	runner container                             started     2026-08-02 21:47:40Z
//
// The document asking for PALAI_RUNNER_CONCURRENCY=4 was written ten hours after the machine it configures
// started, and delivery rode ENROLMENT alone — so that machine had never seen it. The store's own journey
// test wrote the limit down in prose: "A MACHINE ALREADY RUNNING does NOT. Delivery rides enrolment and
// nothing pushes a later revision at a live runner."
//
// AND WHY THESE TESTS NEVER ASSERT THE NUMBER 4. `docker inspect` on that same runner shows
// PALAI_RUNNER_CONCURRENCY=4, because compose sets it and planeIntDefault falls back to the environment
// when the plane sends nothing. The panel and the machine agreed and nothing had travelled between them. A
// test that watched for the desired number could pass on a machine that never received a byte, so what is
// asserted below is the TRANSITION — a value the machine was not started with, arriving while it runs.

// settingsFor builds a Settings func that serves a scripted sequence of documents and records every report
// the machine sends back. It is deliberately not a fake control plane: FetchSettings is exercised against a
// real TLS server elsewhere, and what these tests are about is what the machine DOES with an answer.
func settingsFor(documents []Settings) (func(context.Context, Identity, Settings) (Settings, error), func() []Settings) {
	var (
		mu      sync.Mutex
		calls   int
		reports []Settings
	)
	fetch := func(_ context.Context, _ Identity, report Settings) (Settings, error) {
		mu.Lock()
		defer mu.Unlock()
		reports = append(reports, report)
		document := documents[len(documents)-1]
		if calls < len(documents) {
			document = documents[calls]
		}
		calls++
		return document, nil
	}
	return fetch, func() []Settings {
		mu.Lock()
		defer mu.Unlock()
		return append([]Settings(nil), reports...)
	}
}

// TestARunningMachineAppliesARevisionWrittenAfterItStarted is the whole point of the settings poll.
//
// The machine is started at concurrency 1 — as a machine whose pool had no document would be — and a
// document raising it to 3 is written while it runs. Nothing re-enrols and nothing restarts.
//
// IT ASSERTS THE LEASE POOL AND NOT THE VERDICT, and that distinction is the one this tree keeps paying
// for: a verdict of "applied" is the machine's CLAIM, and a test that only read it back would be asserting
// that the machine says what it says. leases.current() is the number the park loops are actually sized by.
func TestARunningMachineAppliesARevisionWrittenAfterItStarted(t *testing.T) {
	fetch, _ := settingsFor([]Settings{
		{Revision: 7, Settings: map[string]string{"PALAI_RUNNER_CONCURRENCY": "3"}},
	})
	cfg := ServeConfig{Settings: fetch, SettingsInterval: time.Millisecond}
	leases := &leasePool{}
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The machine starts on 1. parkLoop is never entered because Serve is not called — the pool is driven
	// directly, so this test measures the sizing and not the lease protocol.
	leases.resize(ctx, &wg, ServeConfig{}, &serveState{}, func(string, ...any) {}, time.Millisecond, 1)
	if got := leases.current(); got != 1 {
		t.Fatalf("the machine started at concurrency %d, want 1 — the premise of this test is that the "+
			"document below is a value it was NOT started with", got)
	}

	go cfg.settingsLoop(ctx, &serveState{}, leases, &wg, func(string, ...any) {}, time.Millisecond)

	deadline := time.After(5 * time.Second)
	for leases.current() != 3 {
		select {
		case <-deadline:
			t.Fatalf("the machine is still serving %d leases at once after a document asking for 3 was "+
				"published to it. It never re-enrolled and never restarted, which is exactly the case delivery-"+
				"at-enrolment could not serve: an operator edits the panel and the running fleet does not move",
				leases.current())
		case <-time.After(time.Millisecond):
		}
	}
}

// TestTheMachineReportsAppliedForWhatItReadsAndNotReadForWhatItDoesNot pins the verdict vocabulary against
// the applier table, which is the only thing that decides it.
//
// THE SECOND SETTING IS THE ONE THAT MATTERS. PALAI_WORKSPACE_ROOT is read by cmd/runner — from its own
// environment, at exec, never from the plane document — so a document carrying it changes nothing about
// this machine now and nothing after a restart either. Reporting it as `applied` would be a lie a panel
// would repeat; reporting it as `pending_restart` would be a subtler lie, because no restart will apply
// it. `not_read` is the only true answer and the machine is the only party that knows it.
func TestTheMachineReportsAppliedForWhatItReadsAndNotReadForWhatItDoesNot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	leases := &leasePool{}
	leases.resize(ctx, &wg, ServeConfig{}, &serveState{}, func(string, ...any) {}, time.Millisecond, 1)

	verdict := ServeConfig{}.applySettings(ctx, leases, &wg, &serveState{}, func(string, ...any) {}, time.Millisecond,
		map[string]string{
			"PALAI_RUNNER_CONCURRENCY": "2",
			"PALAI_WORKSPACE_ROOT":     "/srv/palai/workspaces",
			"PALAI_DISPATCH_WORKERS":   "4",
		})

	for name, want := range map[string]string{
		"PALAI_RUNNER_CONCURRENCY": VerdictApplied,
		// Read by this binary, but from the ENVIRONMENT at exec — never from the plane document.
		"PALAI_WORKSPACE_ROOT": VerdictNotRead,
		// A control-plane setting entirely. The write path refuses it into a runner document, so a machine
		// should never see one; if it ever does, saying so is better than silence.
		"PALAI_DISPATCH_WORKERS": VerdictNotRead,
	} {
		if got := verdict[name]; got != want {
			t.Errorf("the machine reported %s as %q, want %q. The verdict is what a panel shows an operator "+
				"about a value they saved, so a wrong one is a screen claiming a machine did something it did not",
				name, got, want)
		}
	}
	if got := leases.current(); got != 2 {
		t.Errorf("the machine reported PALAI_RUNNER_CONCURRENCY applied and is serving %d leases at once, want 2. "+
			"A verdict of %q that did not resize is the exact defect the verdict exists to prevent, told by the "+
			"machine instead of by the panel", got, VerdictApplied)
	}
}

// TestAMalformedConcurrencyIsRefusedByNameRatherThanCoerced.
//
// planeIntDefault falls back silently on an unparseable value, which is correct for a value read once at
// boot. It is wrong for one an operator just typed into a panel: the machine would run the old number and
// the screen would show the new one, with nothing anywhere saying they disagree.
func TestAMalformedConcurrencyIsRefusedByNameRatherThanCoerced(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	leases := &leasePool{}
	leases.resize(ctx, &wg, ServeConfig{}, &serveState{}, func(string, ...any) {}, time.Millisecond, 2)

	for _, value := range []string{"0", "-1", "four", "2.5", ""} {
		verdict := ServeConfig{}.applySettings(ctx, leases, &wg, &serveState{}, func(string, ...any) {}, time.Millisecond,
			map[string]string{"PALAI_RUNNER_CONCURRENCY": value})
		// ‼️ NOT `!= VerdictApplied`, WHICH IS WHAT THIS ASSERTED UNTIL 2026-08-06. That form passes on
		// "not_read", on the empty string, and on any word at all — every one of which tells an operator
		// something different from "this machine rejected the number you typed". A refusal is only useful
		// if a reader can RECOGNISE it, and the panel reading it speaks no Go.
		if got := verdict["PALAI_RUNNER_CONCURRENCY"]; !IsRefused(got) {
			t.Errorf("the machine reported %q for PALAI_RUNNER_CONCURRENCY=%q — an operator cannot tell a "+
				"refusal from a setting this build ignores", got, value)
		}
		if got := verdict["PALAI_RUNNER_CONCURRENCY"]; got == VerdictRefused {
			t.Errorf("PALAI_RUNNER_CONCURRENCY=%q was refused with NO reason. The verdict is the operator's "+
				"only account of why, on a machine they may have no shell on", value)
		}
		if got := leases.current(); got != 2 {
			t.Fatalf("PALAI_RUNNER_CONCURRENCY=%q resized the machine to %d. A value this binary's own reader "+
				"would not parse must leave the machine where it was", value, got)
		}
	}
}

// TestTheReportDescribesTheDocumentTheMachineHasAlreadyActedOn.
//
// The first poll of a process's life must report revision 0 and nothing applied — it has been sent nothing
// yet, and a machine that reported verdicts for a document it had not seen would make the panel's
// "applied" mean "intends to". The SECOND poll carries the first document's verdicts.
func TestTheReportDescribesTheDocumentTheMachineHasAlreadyActedOn(t *testing.T) {
	fetch, reports := settingsFor([]Settings{
		{Revision: 11, Settings: map[string]string{"PALAI_RUNNER_CONCURRENCY": "2"}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	leases := &leasePool{}
	leases.resize(ctx, &wg, ServeConfig{}, &serveState{}, func(string, ...any) {}, time.Millisecond, 1)
	cfg := ServeConfig{Settings: fetch, SettingsInterval: time.Millisecond}
	go cfg.settingsLoop(ctx, &serveState{}, leases, &wg, func(string, ...any) {}, time.Millisecond)

	deadline := time.After(5 * time.Second)
	for len(reports()) < 2 {
		select {
		case <-deadline:
			t.Fatalf("only %d polls in five seconds", len(reports()))
		case <-time.After(time.Millisecond):
		}
	}
	cancel()

	got := reports()
	if got[0].Revision != 0 || len(got[0].Settings) != 0 {
		t.Errorf("the first poll reported revision %d with %d verdicts, want 0 and none: a machine that has "+
			"been sent nothing has applied nothing, and claiming otherwise makes the panel's word for `applied` "+
			"mean `was told to`", got[0].Revision, len(got[0].Settings))
	}
	if got[1].Revision != 11 {
		t.Errorf("the second poll reported revision %d, want 11 — the document the machine has by then acted on", got[1].Revision)
	}
	if verdict := got[1].Settings["PALAI_RUNNER_CONCURRENCY"]; verdict != VerdictApplied {
		t.Errorf("the second poll reported PALAI_RUNNER_CONCURRENCY as %q, want %q", verdict, VerdictApplied)
	}
}

// TestShrinkingConcurrencyRetiresLoopsAndNeverBelowOne.
//
// The retirement is COOPERATIVE — a loop leaves after finishing a lease, never mid-engine — so what is
// asserted here is the bookkeeping that drives it rather than a goroutine count: retire() must say yes
// exactly as many times as the pool was shrunk by, and then stop.
func TestShrinkingConcurrencyRetiresLoopsAndNeverBelowOne(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	leases := &leasePool{}
	noop := func(string, ...any) {}
	// Four loops wanted; the pool has started four (they park immediately and are idle).
	leases.resize(ctx, &wg, ServeConfig{}, &serveState{}, noop, time.Millisecond, 4)

	leases.resize(ctx, &wg, ServeConfig{}, &serveState{}, noop, time.Millisecond, 2)
	retired := 0
	for leases.retire() {
		retired++
		if retired > 10 {
			t.Fatal("retire() never stopped saying yes: every park loop would exit and the machine would serve nothing")
		}
	}
	if retired != 2 {
		t.Errorf("%d loops retired after a shrink from 4 to 2, want 2", retired)
	}

	// Zero is the UNSET default rather than "take no work" — a machine that should take none is cordoned,
	// which is a fleet decision with its own durable state. A concurrency of 0 that emptied the pool would
	// look identical to a misconfigured one and would be reported as `applied`.
	leases.resize(ctx, &wg, ServeConfig{}, &serveState{}, noop, time.Millisecond, 0)
	if got := leases.current(); got != 1 {
		t.Errorf("a resize to 0 left the pool wanting %d, want 1", got)
	}
}

// TestEveryVerdictThisBinaryCanReportIsOneOfTheDECLAREDForms is the vocabulary's own guard, and it drives
// the REAL apply path rather than listing the constants back.
//
// ‼️ IT EXISTS BECAUSE THE REFUSAL WAS NOT A CONSTANT. `"refused: not a positive integer"` was a literal at
// one call site, so the wire form was whatever that line happened to say — a second refusal arm would have
// written "rejected", "invalid" or "refused (not an integer)", and each would have reached the panel as a
// verdict nothing could group with the others. What is asserted is the SHAPE of every answer the switch can
// give, over settings this build reads, settings it does not, and a value it will not take.
func TestEveryVerdictThisBinaryCanReportIsOneOfTheDECLAREDForms(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	leases := &leasePool{}
	leases.resize(ctx, &wg, ServeConfig{}, &serveState{}, func(string, ...any) {}, time.Millisecond, 1)

	verdict := ServeConfig{}.applySettings(ctx, leases, &wg, &serveState{}, func(string, ...any) {}, time.Millisecond,
		map[string]string{
			"PALAI_RUNNER_CONCURRENCY": "3",          // read, and takeable
			"PALAI_WORKSPACE_ROOT":     "/srv/palai", // catalogued on the plane, not read by this binary
		})
	verdict["refused"] = ServeConfig{}.applySettings(ctx, leases, &wg, &serveState{}, func(string, ...any) {}, time.Millisecond,
		map[string]string{"PALAI_RUNNER_CONCURRENCY": "four"})["PALAI_RUNNER_CONCURRENCY"]

	for name, got := range verdict {
		switch {
		case got == VerdictApplied, got == VerdictNotRead, IsRefused(got):
		default:
			t.Errorf("%s reported %q, which is none of the declared verdicts (%q, %q, %q-prefixed). A verdict "+
				"nothing declares is a word only its author can read", name, got, VerdictApplied, VerdictNotRead, VerdictRefused)
		}
	}

	// And the classifier separates them, so it cannot pass by calling everything a refusal.
	if IsRefused(VerdictApplied) || IsRefused(VerdictNotRead) || IsRefused("") {
		t.Error("IsRefused accepts a verdict that is not a refusal — a classifier that says yes to everything sorts nothing")
	}
}
