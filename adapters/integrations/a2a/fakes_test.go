package a2a

import (
	"context"
	"net/http"
	"sync"
)

// The conformance/loopback tier's fakes. They are named FAKE deliberately: this proves canonical mapping +
// protocol shape against an in-repo server driven by a generic loopback HTTP client — NOT a foreign-peer
// interop claim (that is the §6 operator leg; the capability stays "preview").

// scopeFromHeader is the conformance ScopeFunc: it reads a deterministic tenant from test headers, standing
// in for the bearer identity the real auth middleware establishes. No header ⇒ unauthenticated (401 on authed
// routes). It is the ONLY identity authority — a forged org/project in a message body never reaches it.
func scopeFromHeader(r *http.Request) (Scope, bool) {
	org := r.Header.Get("X-Scope-Org")
	if org == "" {
		return Scope{}, false
	}
	return Scope{Organization: org, Project: r.Header.Get("X-Scope-Project")}, true
}

type fakeInterfaces struct {
	byID map[string]PublishedInterface
}

func (f *fakeInterfaces) ResolvePublic(_ context.Context, id string) (PublishedInterface, bool, error) {
	iface, ok := f.byID[id]
	return iface, ok, nil
}

func (f *fakeInterfaces) Get(_ context.Context, org, project, id string) (PublishedInterface, bool, error) {
	iface, ok := f.byID[id]
	if !ok || iface.Organization != org || iface.Project != project {
		return PublishedInterface{}, false, nil // foreign scope is an indistinguishable miss
	}
	return iface, true, nil
}

// admitCall records exactly what identity a run was admitted under — the endpoint-level identity-override
// assertion reads it to prove metadata never governed.
type admitCall struct {
	Org, Project, Input, IdempotencyKey, AgentRevisionID string
}

type fakeRuns struct {
	mu       sync.Mutex
	admits   []admitCall
	result   RunResult // the RunResult Admit returns (drives Task vs direct-message + state mapping)
	byRun    map[string]RunResult
	cancelTo RunResult
	cancelSE string // an uncertain non-cancelable side-effect to surface (§38.3)
}

func (f *fakeRuns) Admit(_ context.Context, req RunRequest) (RunResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.admits = append(f.admits, admitCall{req.Org, req.Project, req.Input, req.IdempotencyKey, req.AgentRevisionID})
	res := f.result
	if f.byRun == nil {
		f.byRun = map[string]RunResult{}
	}
	f.byRun[res.RunID] = res
	return res, nil
}

func (f *fakeRuns) Get(_ context.Context, _, _, runID string) (RunResult, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	res, ok := f.byRun[runID]
	return res, ok, nil
}

func (f *fakeRuns) Cancel(_ context.Context, _, _, runID string) (RunResult, CancelReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byRun[runID] = f.cancelTo
	return f.cancelTo, CancelReport{UncertainSideEffect: f.cancelSE}, nil
}

func (f *fakeRuns) lastAdmit() admitCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.admits[len(f.admits)-1]
}

type fakeTasks struct {
	mu   sync.Mutex
	byID map[string]TaskRef // key: interfaceID + "\x00" + a2aTaskID
}

func taskKey(interfaceID, a2aTaskID string) string { return interfaceID + "\x00" + a2aTaskID }

func (f *fakeTasks) Put(_ context.Context, _, _ string, ref TaskRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.byID == nil {
		f.byID = map[string]TaskRef{}
	}
	f.byID[taskKey(ref.InterfaceID, ref.A2ATaskID)] = ref
	return nil
}

func (f *fakeTasks) GetRef(_ context.Context, _, _, interfaceID, a2aTaskID string) (TaskRef, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ref, ok := f.byID[taskKey(interfaceID, a2aTaskID)]
	return ref, ok, nil
}

func (f *fakeTasks) List(_ context.Context, _, _, interfaceID string, _ int) ([]TaskRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []TaskRef
	for _, ref := range f.byID {
		if ref.InterfaceID == interfaceID {
			out = append(out, ref)
		}
	}
	return out, nil
}

func (f *fakeTasks) SetPushConfigs(_ context.Context, _, _, interfaceID, a2aTaskID string, cfgs []PushNotificationConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := taskKey(interfaceID, a2aTaskID)
	ref := f.byID[k]
	ref.PushConfigs = cfgs
	f.byID[k] = ref
	return nil
}

type fakeFiles struct {
	mu       sync.Mutex
	ingested []FilePart
}

func (f *fakeFiles) Ingest(_ context.Context, _, _, _ string, p FilePart) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ingested = append(f.ingested, p)
	return "artf_" + p.Name, nil
}

func (f *fakeFiles) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ingested)
}
