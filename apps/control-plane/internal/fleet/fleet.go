// Package fleet is the runner registry: which machines have enrolled, under which pool, holding
// which certificate, and when each was last seen. It is the table runner_gateway.go:73 and
// local_credentials.go:122 each independently named as the missing upgrade path, and it lives in
// its own package for a boundary reason rather than a tidiness one — internal/execution is already
// forty-odd files, and putting a Postgres store inside RunnerGateway would make the gateway depend
// on the database. The gateway takes the Registry INTERFACE: Postgres in production, a fake in the
// wire proofs.
//
// HONEST CEILING, and it is the whole scope of E24 T1: this is an INVENTORY, not a health source.
// LastSeenAt advances on enrol, connect and renew — the three moments a runner proves it is alive
// by authenticating — and NOTHING here polls, reaps, or expires a row. A runner whose machine was
// unplugged keeps its `active` state and a stale LastSeenAt until something else writes. Heartbeat
// and the reaper are T5's work; naming this a health source before then would be a lie the next
// reader would have to discover.
package fleet

import (
	"context"
	"errors"
	"time"
)

// ErrUnknownPool is returned by Register when the named pool does not exist in the scope the
// registration runs under. Enrolling into a pool that is not there must be a refusal and not a row
// with a dangling pool_id: placement (T2/T4) reads the pool to decide WHERE a run goes, and a
// runner in no pool is a machine no decision can reach.
var ErrUnknownPool = errors.New("fleet: no such runner pool")

// ErrIdentityMismatch is returned by Register when a Registration's DNS is not derived from its ID.
// It is a structural guard against the divergence that shipped once already: the row is looked up by
// certificate SAN for the rest of its life, so a row whose recorded SAN names a different machine than
// its id is a row nothing can ever find again. The store checks the pairing rather than owning the
// suffix, so the DNS contract stays where the CA and the runner already agree on it.
var ErrIdentityMismatch = errors.New("fleet: runner DNS is not derived from the runner id")

// ErrCapacityNotDeclarable is returned by Register for a NEGATIVE declared capacity (Faz A.4 T5). Zero is
// legal and means "declared nothing"; a positive number is a ceiling. A negative one is neither, and the
// column's CHECK would refuse it anyway — refusing it here turns a constraint violation the operator
// cannot read into a refusal that names what they got wrong.
var ErrCapacityNotDeclarable = errors.New("fleet: declared capacity cannot be negative")

// ErrUnknownLifecycleAction is returned by SetState for a verb that is not cordon/resume/revoke. The
// action reaches the store from a URL path segment and `runners.state` is CHECK-constrained, so an
// unmapped verb has to be refused by the surface that took it rather than by Postgres.
var ErrUnknownLifecycleAction = errors.New("fleet: unknown runner lifecycle action")

// ErrRunnerRevoked is returned by Register when the enrolling device key's fingerprint belongs to a row
// an operator REVOKED.
//
// ‼️ IT IS WHAT MAKES A DECOMMISSIONING SURVIVE THE MACHINE STILL HOLDING A LIVE POOL KEY. A pool key is
// reusable by design — it enrols a fleet, not a box — so before device keys, revoking a Mac and leaving
// the key on it meant the Mac came back on its next restart under a NEW id and no revocation reached it.
// The fingerprint is the thing the revocation actually named, so presenting it recovers the revoked row
// and is refused rather than minting a second identity for the same hardware.
var ErrRunnerRevoked = errors.New("fleet: this device's runner identity has been revoked")

// ErrIdentityNotRecoverable is returned by Register when a machine claims a runner id its device key
// cannot support: either the fingerprint resolves to a DIFFERENT row, or it resolves to none at all.
//
// THE SECOND SHAPE IS THE ONE WORTH NAMING. A machine whose disk kept the identity file and lost the key
// (a re-image, a restored backup) presents an id it cannot prove, and honouring that claim would be a way
// to BECOME another machine by copying one small JSON file. It is refused, and a genuinely new install
// with a genuinely new key — which presents no claim — becomes a new machine instead.
var ErrIdentityNotRecoverable = errors.New("fleet: the claimed runner identity does not belong to this device key")

// ErrIsolationUnsupported is returned by Register when the machine's MEASURED isolation modes do not
// include the one its pool requires (plan §3.5, DoD 9).
//
// IT IS THE ONE REFUSAL ON THIS PATH BUILT ON A MEASUREMENT RATHER THAN A DECLARATION. Posture, pool and
// capacity are all claims the control plane records and cannot verify; whether palai-agentd answered a
// socket is something the machine found out. Refusing here rather than at placement is the property DoD 9
// states: a machine that cannot execute never appears as ready capacity.
var ErrIsolationUnsupported = errors.New("fleet: the machine does not support the isolation mode this pool requires")

// RunnerLifecycle is the LIVE half of a lifecycle decision: the gateway holding that machine's sessions.
// The durable half is a row, and a row alone would take effect only at the machine's next connect — which
// for a cordoned Mac serving a two-hour run is two hours away.
//
// THE INTERFACE LIVES HERE AND THE IMPLEMENTATION IS *execution.RunnerGateway, which is the direction the
// dependency has to run: internal/execution imports internal/fleet, so fleet cannot import it back. One
// method per verb rather than one `SetState(id, action)` because the gateway's three are genuinely
// different operations — one evicts, one re-admits, one cuts — and collapsing them would mean re-deriving
// which is which inside the gateway from a string this package already parsed.
type RunnerLifecycle interface {
	CordonRunner(runnerID string)
	ResumeRunner(runnerID string)
	RevokeRunner(runnerID string)
	// ApproveRunner admits a machine held in a strict pool's waiting room (E24 T6). It is on this interface
	// and not a second one because it is the same kind of fact as the other three — a decision the ROW
	// records and the live gateway has to be told about — and the reason it needs telling is sharper here
	// than for a cordon: the machine is holding a session open right now, waiting for exactly this.
	ApproveRunner(runnerID string)
	// RunnerActiveLeases is how many leases that machine is serving right now — the answer to the question
	// a cordon exists to let an operator ask, which is whether the Mac can be taken away yet.
	RunnerActiveLeases(runnerID string) int64
	// Waiting is how many attempts are queued for a POOL with no machine free to take them (E28 T1's rider,
	// closing `FLT-P14`). It is on this interface because this is already the seam through which a stored
	// fact meets a live one, and the gateway already had the method — what it had never had was a reader.
	Waiting(poolID string) int
}

// DefaultPoolID is the pool every runner enrols into until enrollment carries a tenant of its own.
//
// WHY A CONSTANT AND NOT A LOOKUP: today's enrollment request is {runner_id, public_key} — no org,
// no project, no pool (§3.6 D8). There is therefore nothing on the wire to resolve a pool FROM, and
// inventing one would be inventing a tenant. The bootstrap deployment's pool is seeded by migration
// 000045 R6 and by the identity bootstrap, so the single-runner install keeps working unchanged;
// T3's pool key is what first puts a pool (and with it a tenant) on the enrollment wire.
const DefaultPoolID = "pool_default"

// Runner is one enrolled machine. The ID is SERVER-MINTED (`rnr_`), which is the property the whole
// registry rests on: the client-supplied name is Label and decides nothing, so two machines that
// both call themselves "runner-local" are two rows and two certificates.
type Runner struct {
	ID      string
	Project string
	PoolID  string
	// Label is what the enrolling machine called itself (PALAI_RUNNER_ID). It is operator-facing
	// information only — never an identity, never a lookup key, never unique.
	Label string
	// DNS is the SAN of the certificate the gateway issued, derived from ID. It is the only identity
	// a later request carries (a renew presents a certificate, not an id), so it is how RecordSeen
	// finds the row.
	DNS string
	// PublicKeySHA256 fingerprints the key the machine enrolled with, so an operator can tell a
	// re-enrolled machine from a different one. It is a public half of a credential, never a secret.
	PublicKeySHA256 string
	State           string
	OS              string
	Arch            string
	Posture         string
	Capacity        int
	CertNotAfter    time.Time
	EnrolledAt      time.Time
	LastSeenAt      time.Time
	// THE MACHINE'S OWN ANSWER about the configuration it holds (migration 000060). These three are the
	// only fields on this struct the CONTROL PLANE does not decide: they are written by the settings poll
	// from what the machine reported, and they exist so a panel can distinguish "the operator saved this"
	// from "the machine is running it".
	//
	// ConfigRevision is the desired revision this machine resolved, 0 when it has never reported — every
	// runner enrolled before 000060, and every runner too old to poll. ConfigApplied is its verdict per
	// setting (`applied` / `not_read`), nil when it has never reported and possibly EMPTY when it has, which
	// is a different fact: the machine polled and the plane had no document for it.
	ConfigRevision   int64
	ConfigApplied    map[string]string
	ConfigReportedAt time.Time
	// AgentVersion and IsolationModes are what the machine REPORTED at its last enrolment — 000007's two
	// columns. They are read here because a column written and used in no decision is the defect this
	// tree has already paid for once with runners.capacity: the panel could not say what build a machine
	// was running or whether it could isolate a session, while the answer sat in the row.
	AgentVersion   string
	IsolationModes string
	// CreatedAt is the keyset coordinate the list cursor is minted from (api/pagination.go orders on
	// (created_at, id) DESC). It is the 000001 column; EnrolledAt is 000045's and records the same
	// instant for every row this code writes, but the page orders on the former.
	CreatedAt time.Time
}

// Registration is what the gateway knows at enrol time.
//
// ID AND DNS ARRIVE TOGETHER AND THE STORE REFUSES THEM APART, which is a correction to how this type
// was first written. It carried no ID, on the reasoning that minting one is the registry's job — and
// the result was TWO minters, because the caller still had to derive a certificate name from
// something before it had a row. The row then recorded one machine's name and the CA signed another's.
// One minter is the invariant; the caller is it, because the caller is what has to name the
// certificate, and Register enforces the pairing rather than trusting it.
type Registration struct {
	// ID is the SERVER-minted runner id (`rnr_`). It is never anything the enrolling machine sent —
	// that is Label — and DNS must be derived from it.
	ID              string
	PoolID          string
	Label           string
	DNS             string
	PublicKeySHA256 string
	OS              string
	Arch            string
	// Posture is what the enrolling machine SAYS it is ('sandboxed-linux' / 'unsandboxed-host'). It is
	// compared against the pool's and a mismatch REFUSES the enrolment (ErrPostureMismatch) — see
	// pools.go for the difference between catching a mismatch and verifying a claim, which is the
	// ceiling this field carries. Empty declares nothing and is what every runner built before E24
	// sends, so it inherits the pool's posture and enrols exactly as it did.
	Posture string
	// Capacity is how many OCCUPANCIES the machine may hold at once — how many sessions' allocations
	// live on it simultaneously, which outlasts any one lease. Since Faz A.4 T5 it is enforced, in one
	// place: coordinator.AcquireLease refuses a hold that would put the machine over it
	// (ErrMachineAtCapacity), decided inside the INSERT so a tie has a loser.
	//
	// IT IS STILL NOT A NUMBER ANY MACHINE SENDS, and that is worth knowing before reading a stored 1 as
	// a declaration. Measured 2026-08-05: the one production Registration (runner_gateway.go's enrolment
	// handler) sets every other field and not this one, `packages/runner` never mentions capacity, and
	// storage/queries/runners.sql has no UPDATE that could change it afterwards — so every machine in
	// every deployment carries the 1 store.go's clamp writes for an absent value.
	//
	// The gateway is a separate accounting and remains unconnected to this: its per-pool channels are
	// unbuffered and its per-machine `active` counts in-flight LEASES, not occupancies (§3.6 D13).
	Capacity int
	// KeyID is the pool enrolment key that admitted this machine (E24 T3), written to
	// runners.enrolled_via_key_id and to the journal entry. EMPTY means the file bootstrap token, which
	// is not a row and therefore cannot be revoked on its own — that is a fact about the file token, not
	// a missing value, so it is stored as NULL rather than as a sentinel id pointing at nothing.
	//
	// It is the fact §3.6 D5 names as the one missing piece of targeted revocation: revoking a key was
	// all-or-nothing because nothing recorded which key issued which certificate.
	KeyID string
	// Version is the agent build the machine reported. Inventory: the support window is enforced at
	// connect, not at enrolment.
	Version string
	// IsolationModes is what the machine MEASURED it can provide. It is checked against the pool's
	// required mode before the row is written; empty declares nothing and a pool with no requirement
	// admits it, which is every pool that exists today.
	IsolationModes []string
	// RecoverRunnerID is the identity the machine says it already holds (plan §3.4). EMPTY asks for a new
	// one; non-empty is a CLAIM, and Register refuses it unless the fingerprint resolves to exactly that
	// row. It is never the id written — that comes from the row the fingerprint found.
	RecoverRunnerID string
}

// ListWindow is the keyset page window the read surface passes down, the shape api.ListQuery
// resolves to. Limit is the over-fetch (page size + 1) so the handler can detect a further page.
type ListWindow struct {
	CreatedGTE     *time.Time
	CreatedLTE     *time.Time
	AfterCreatedAt *time.Time
	AfterID        string
	Limit          int
}

// Registry is the four-method seam. The gateway drives Register and RecordSeen; the read surface
// drives Get and List. One interface rather than two because there is one implementation and one
// fake, and splitting it would be an abstraction bought before anything asked for it.
//
// Register and RecordSeen take NO tenant: the runner plane does not carry one yet (§3.6 D8), so the
// store resolves the tenant from the POOL the machine enrols into — which is exactly why an unknown
// pool has to be a refusal. Get and List DO take one: they are reached through the public API, where
// the verified bearer scope is the only tenant authority.
type Registry interface {
	// Register records the machine under the id the server minted and appends the `issued`
	// enrollment-journal entry in ONE transaction — a certificate the journal does not record is a
	// certificate no revoke can find. Returns ErrUnknownPool for a pool that is not there and
	// ErrIdentityMismatch for a DNS that is not derived from the id.
	Register(ctx context.Context, reg Registration) (Runner, error)
	// RecordSeen advances LastSeenAt (and the certificate expiry) for the runner holding dns, and
	// reports whether such a row exists. It is called on connect and on renew — the two moments a
	// machine proves liveness by authenticating with its certificate — and it is deliberately NOT a
	// health check: it writes what just happened, it does not judge what did not.
	RecordSeen(ctx context.Context, dns string, certNotAfter time.Time, at time.Time) (Runner, bool, error)
	// Pool resolves one pool by id, SYSTEM-SCOPED, and is the answer to "whose machine is this" for a
	// machine the registry has no row for — a runner that enrolled before E24 and has not restarted
	// since (E24 T4). It takes no tenant for the same reason Register does not: on this plane the pool
	// is what RESOLVES a tenant, so asking for one would be asking the caller to already know the
	// answer. It is a by-primary-key read of a row an operator created, not caller input.
	Pool(ctx context.Context, poolID string) (Pool, bool, error)
	Get(ctx context.Context, project, id string) (Runner, bool, error)
	List(ctx context.Context, project string, window ListWindow) ([]Runner, error)
}
