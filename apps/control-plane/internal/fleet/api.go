package fleet

import (
	"context"
	"errors"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/coordinator"
)

// The adapter that makes Store the api.RunnerRegistryAPI the read routes take. It is a projection and
// nothing more: no field is computed here, and in particular nothing is derived that would look like
// health. The registry records when a machine last authenticated; deciding what that MEANS is T5's,
// and a `healthy` bool invented in this file would be a lie with a type.

// ListRunners implements api.RunnerRegistryAPI. Organization is resolved fresh from project (A.2 Task 3):
// the request scope no longer carries one.
func (s *Store) ListRunners(ctx context.Context, project string, w api.RunnerListWindow) ([]api.RunnerItem, error) {
	rows, err := s.List(ctx, project, ListWindow{
		CreatedGTE: w.CreatedGTE, CreatedLTE: w.CreatedLTE,
		AfterCreatedAt: w.AfterCreatedAt, AfterID: w.AfterID, Limit: w.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]api.RunnerItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, runnerItem(row))
	}
	return out, nil
}

// GetRunner implements api.RunnerRegistryAPI. Shadowed by RegistryAPI.GetRunner below in production; kept
// consistent with the interface for the same reason that one is.
func (s *Store) GetRunner(ctx context.Context, project, id string) (api.RunnerItem, bool, error) {
	row, found, err := s.Get(ctx, project, id)
	if err != nil || !found {
		return api.RunnerItem{}, false, err
	}
	return runnerItem(row), true, nil
}

// ListRunnerPools implements api.RunnerRegistryAPI (E24 T2). Shadowed by RegistryAPI.ListRunnerPools below.
func (s *Store) ListRunnerPools(ctx context.Context, project string, w api.RunnerListWindow) ([]api.RunnerPoolItem, error) {
	rows, err := s.ListPools(ctx, project, ListWindow{
		CreatedGTE: w.CreatedGTE, CreatedLTE: w.CreatedLTE,
		AfterCreatedAt: w.AfterCreatedAt, AfterID: w.AfterID, Limit: w.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]api.RunnerPoolItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, poolItem(row))
	}
	return out, nil
}

// poolItem projects one pool. `Waiting` is deliberately not set here: it is not a stored fact, and the only
// thing that knows it is the live gateway — see RegistryAPI.ListRunnerPools, which is the one place a
// runtime fact joins a stored one on this surface.
func poolItem(row Pool) api.RunnerPoolItem {
	return api.RunnerPoolItem{
		ID: row.ID, Project: row.Project, Name: row.Name, Posture: row.Posture, OS: row.OS, Arch: row.Arch,
		StrictEnrollment: row.StrictEnrollment, CreatedAt: row.CreatedAt,
	}
}

// THE KEY SURFACE ADAPTER (E24 T3). It hangs off PoolEnrollmentKeys rather than Store because the two
// are different concerns — inventory and credential — and the router takes ONE interface, so the
// composition root passes a small struct that holds both. That is cheaper than merging the types and it
// keeps the credential path out of every caller that only wants to list machines.

// RegistryAPI is the api.RunnerRegistryAPI implementation: the registry's reads plus the key surface's
// writes, joined where the router needs them joined and nowhere deeper.
type RegistryAPI struct {
	*Store
	keys *PoolEnrollmentKeys
	// live is the gateway (E24 T5). Nil leaves the lifecycle routes writing the ROW only, which is a
	// supported posture rather than a fallback: a control plane with no runner listener bound has no
	// sessions to cordon, and a decision recorded now is adopted by whichever gateway that machine next
	// connects to.
	live RunnerLifecycle
}

// NewRegistryAPI joins the two. keys may be nil, in which case the key routes answer as if no pool had
// keys — a deployment that wires no key store has none.
func NewRegistryAPI(store *Store, keys *PoolEnrollmentKeys) *RegistryAPI {
	return &RegistryAPI{Store: store, keys: keys}
}

// WithLifecycle wires the live gateway a lifecycle decision has to reach as well as the row. A setter for
// the reason SetRegistry is one: the gateway is built after the stores are, and a deployment that binds no
// runner listener passes nothing.
func (a *RegistryAPI) WithLifecycle(live RunnerLifecycle) *RegistryAPI {
	a.live = live
	return a
}

// SetRunnerState implements api.RunnerRegistryAPI: cordon, resume or revoke ONE machine.
//
// THE ROW FIRST, THEN THE LIVE GATEWAY, and the order is the whole of "a revocation survives a restart".
// A decision that reached only the gateway is a decision the next restart erases (§3.6 D15); a decision
// that reached only the row takes effect at the machine's next connect, which for a Mac mid-run is hours
// away. Both, in that order, means a crash between them leaves the safe state: recorded but not yet
// applied, which the machine's next connect resolves because handleConnect adopts the row.
func (a *RegistryAPI) SetRunnerState(ctx context.Context, project, id, action string) (api.RunnerItem, bool, error) {
	row, found, err := a.Store.SetState(ctx, project, id, action)
	if errors.Is(err, ErrUnknownLifecycleAction) {
		return api.RunnerItem{}, false, err
	}
	if err != nil || !found {
		return api.RunnerItem{}, false, err
	}
	if a.live != nil {
		switch action {
		case "cordon":
			a.live.CordonRunner(row.ID)
		case "resume":
			a.live.ResumeRunner(row.ID)
		case "revoke":
			a.live.RevokeRunner(row.ID)
		}
	}
	return a.decorate(row), true, nil
}

// ApproveRunner implements api.RunnerRegistryAPI: admit ONE machine out of a strict pool's waiting room
// (E24 T6).
//
// THE PRINCIPAL IS DERIVED HERE, from the verified key and nowhere else — `key:<api_key_id>`,
// coordinator.ApproverPrincipal's HTTP form, the same string store.DecideApproval stamps. That is why this
// method takes the whole middleware.Scope while the lifecycle verbs take org/project strings: a decision
// carries an identity, and a caller that could pass one would be a caller that could name somebody else.
//
// THE ROW FIRST, THEN THE LIVE GATEWAY, for SetRunnerState's reason exactly: a decision that reached only
// the gateway is erased by the next restart, and one that reached only the row leaves the machine's own
// session still waiting. The gateway is told on every FOUND approval rather than only on the one that moved
// the row — see RunnerGateway.ApproveRunner for the race that makes the retry the fix.
func (a *RegistryAPI) ApproveRunner(ctx context.Context, scope middleware.Scope, id string) (api.RunnerItem, api.ApprovalOutcome, error) {
	admission, err := a.Store.Approve(ctx, scope.Project, id,
		coordinator.ApproverPrincipal(coordinator.ApproverSurfaceKey, "", scope.APIKeyID))
	switch {
	case errors.Is(err, coordinator.ErrApproverNotAuthorized):
		// A TYPED refusal rather than an error: the receiver worked and the request authorized nothing, which
		// is the ApprovalOutcome convention this reuses rather than restates.
		return api.RunnerItem{}, api.ApprovalOutcome{Found: true, Unauthorized: true}, nil
	case err != nil:
		return api.RunnerItem{}, api.ApprovalOutcome{}, err
	case !admission.Found:
		return api.RunnerItem{}, api.ApprovalOutcome{}, nil
	}
	if a.live != nil {
		a.live.ApproveRunner(admission.Runner.ID)
	}
	return a.decorate(admission.Runner), api.ApprovalOutcome{Found: true, Applied: admission.Admitted}, nil
}

// CreateRunnerPool implements api.RunnerRegistryAPI (E28 T1). The name collision comes back as the api
// package's typed value so the handler renders a 409 without parsing a SQLSTATE — and it is translated
// HERE rather than raised there because this is the only layer that has seen the driver's error.
func (s *Store) CreateRunnerPool(ctx context.Context, project string, in api.RunnerPoolCreate) (api.RunnerPoolItem, error) {
	row, err := s.CreatePool(ctx, project, Pool{
		Name: in.Name, Posture: in.Posture, OS: in.OS, Arch: in.Arch, StrictEnrollment: in.StrictEnrollment,
	})
	if errors.Is(err, ErrPoolNameTaken) {
		return api.RunnerPoolItem{}, api.ErrRunnerPoolNameTaken
	}
	if err != nil {
		return api.RunnerPoolItem{}, err
	}
	return poolItem(row), nil
}

// SetRunnerPoolStrictEnrollment implements api.RunnerRegistryAPI (E28 T1).
func (s *Store) SetRunnerPoolStrictEnrollment(ctx context.Context, project, poolID string, strict bool) (api.RunnerPoolItem, bool, error) {
	row, found, err := s.SetStrictEnrollment(ctx, project, poolID, strict)
	if err != nil || !found {
		return api.RunnerPoolItem{}, false, err
	}
	return poolItem(row), true, nil
}

// ListRunnerPools shadows the embedded Store's so the page carries each pool's LIVE queue depth (E28 T1's
// rider, closing `FLT-P14`). `RunnerGateway.Waiting(poolID)` had counted the attempts queued for a pool with
// no free machine since E24 and NOTHING had ever read it, so the question it was written to answer — why is
// nothing running in my Mac pool — still had no answer.
//
// IT IS ON THE LIST RATHER THAN A SINGLE READ, unlike ActiveLeases, and the difference is that there is no
// single-pool route: the gap row named this placement itself ("the authenticated runner-pool read"). What
// it borrows from ActiveLeases verbatim is the POINTER, because a deployment that bound no runner listener
// could not ask, and "could not ask" is not "nothing is waiting".
func (a *RegistryAPI) ListRunnerPools(ctx context.Context, project string, w api.RunnerListWindow) ([]api.RunnerPoolItem, error) {
	items, err := a.Store.ListRunnerPools(ctx, project, w)
	if err != nil || a.live == nil {
		return items, err
	}
	for i := range items {
		waiting := int64(a.live.Waiting(items[i].ID))
		items[i].Waiting = &waiting
	}
	return items, nil
}

// GetRunner shadows the embedded Store's so a single machine's read carries its LIVE lease count. It is
// the per-runner counter's operator-facing consumer: after cordoning a Mac, "how many leases is it still
// serving" is the only question left before unplugging it, and before this there was no way to ask.
func (a *RegistryAPI) GetRunner(ctx context.Context, project, id string) (api.RunnerItem, bool, error) {
	row, found, err := a.Store.Get(ctx, project, id)
	if err != nil || !found {
		return api.RunnerItem{}, false, err
	}
	return a.decorate(row), true, nil
}

// ListRunners is the Store's page with the live gateway's answer joined to every row.
//
// ‼️ IT IS OVERRIDDEN HERE BECAUSE THE SCREEN IS A LISTING. RegistryAPI embeds Store, so without this
// method the page fell through undecorated and the Fleet screen — the surface DoD 10 names — showed no
// connection state at all, while a single read of one machine showed it. That asymmetry was measured
// 2026-08-06, after the field had already been verified live on GetRunner alone.
func (a *RegistryAPI) ListRunners(ctx context.Context, project string, w api.RunnerListWindow) ([]api.RunnerItem, error) {
	items, err := a.Store.ListRunners(ctx, project, w)
	if err != nil || a.live == nil {
		return items, err
	}
	for i := range items {
		leases := a.live.RunnerActiveLeases(items[i].ID)
		items[i].ActiveLeases = &leases
		connections := a.live.RunnerConnections(items[i].ID)
		items[i].Connections = &connections
		items[i].ConnectionRefusal, items[i].ConnectionRefusedAt = a.live.RunnerRefusal(items[i].ID)
	}
	return items, nil
}

// decorate adds what only the live gateway knows. It is deliberately the ONLY place a runtime fact joins a
// stored one, so the read projection cannot grow a field whose source nobody can name.
func (a *RegistryAPI) decorate(row Runner) api.RunnerItem {
	item := runnerItem(row)
	if a.live != nil {
		leases := a.live.RunnerActiveLeases(row.ID)
		item.ActiveLeases = &leases
		connections := a.live.RunnerConnections(row.ID)
		item.Connections = &connections
		item.ConnectionRefusal, item.ConnectionRefusedAt = a.live.RunnerRefusal(row.ID)
	}
	return item
}

// MintRunnerPoolKey implements api.RunnerRegistryAPI.
func (a *RegistryAPI) MintRunnerPoolKey(ctx context.Context, project, poolID string, expiresAt *time.Time) (api.RunnerPoolKeyItem, bool, error) {
	if a.keys == nil {
		return api.RunnerPoolKeyItem{}, false, nil
	}
	minted, err := a.keys.Mint(ctx, project, poolID, expiresAt)
	if errors.Is(err, ErrUnknownPool) {
		return api.RunnerPoolKeyItem{}, false, nil
	}
	if err != nil {
		return api.RunnerPoolKeyItem{}, false, err
	}
	return api.RunnerPoolKeyItem{
		ID: minted.ID, PoolID: minted.PoolID, Prefix: minted.Prefix, Value: minted.Value,
		CreatedAt: minted.CreatedAt, ExpiresAt: minted.ExpiresAt,
	}, true, nil
}

// ListRunnerPoolKeys implements api.RunnerRegistryAPI.
func (a *RegistryAPI) ListRunnerPoolKeys(ctx context.Context, project, poolID string) ([]api.RunnerPoolKeyItem, error) {
	if a.keys == nil {
		return []api.RunnerPoolKeyItem{}, nil
	}
	rows, err := a.keys.List(ctx, project, poolID)
	if err != nil {
		return nil, err
	}
	out := make([]api.RunnerPoolKeyItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, poolKeyItem(row))
	}
	return out, nil
}

// RevokeRunnerPoolKey implements api.RunnerRegistryAPI.
func (a *RegistryAPI) RevokeRunnerPoolKey(ctx context.Context, project, keyID string) (api.RunnerPoolKeyItem, bool, error) {
	if a.keys == nil {
		return api.RunnerPoolKeyItem{}, false, nil
	}
	revoked, err := a.keys.Revoke(ctx, project, keyID)
	if errors.Is(err, ErrUnknownPoolKey) {
		return api.RunnerPoolKeyItem{}, false, nil
	}
	if err != nil {
		return api.RunnerPoolKeyItem{}, false, err
	}
	item := poolKeyItem(revoked.Key)
	// Always non-nil, including when it is empty: "this key admitted no machine" and "we did not look"
	// are different answers, and the renderer distinguishes them by nil.
	item.EnrolledRunners = make([]api.RunnerPoolKeyEnrollment, 0, len(revoked.EnrolledRunners))
	for _, m := range revoked.EnrolledRunners {
		item.EnrolledRunners = append(item.EnrolledRunners, api.RunnerPoolKeyEnrollment{
			ID: m.ID, Label: m.Label, DNS: m.DNS, State: m.State, PoolID: m.PoolID, EnrolledAt: m.EnrolledAt,
		})
	}
	return item, true, nil
}

// poolKeyItem projects a key's metadata. There is no branch here that could carry a value: PoolKey does
// not have one, which is the type system doing the work a review would otherwise have to.
func poolKeyItem(row PoolKey) api.RunnerPoolKeyItem {
	return api.RunnerPoolKeyItem{
		ID: row.ID, PoolID: row.PoolID, Prefix: row.Prefix, CreatedAt: row.CreatedAt,
		ExpiresAt: row.ExpiresAt, RevokedAt: row.RevokedAt, LastUsedAt: row.LastUsedAt,
	}
}

func runnerItem(r Runner) api.RunnerItem {
	return api.RunnerItem{
		ID: r.ID, PoolID: r.PoolID, Label: r.Label, DNS: r.DNS, PublicKeySHA256: r.PublicKeySHA256,
		State: r.State, OS: r.OS, Arch: r.Arch, Posture: r.Posture, Capacity: r.Capacity,
		CertNotAfter: r.CertNotAfter, EnrolledAt: r.EnrolledAt, LastSeenAt: r.LastSeenAt,
		CreatedAt: r.CreatedAt,
		// The machine's own configuration report travels the whole way to the screen unaltered. This
		// projection is the one place the two shapes meet, so a field dropped here is a field that exists in
		// the database and on the API type and appears on no response — the shape this tree keeps finding.
		ConfigRevision: r.ConfigRevision, ConfigApplied: r.ConfigApplied, ConfigReportedAt: r.ConfigReportedAt,
		AgentVersion: r.AgentVersion, IsolationModes: r.IsolationModes,
	}
}
