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
	ID           string
	Organization string
	Project      string
	PoolID       string
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
	// CreatedAt is the keyset coordinate the list cursor is minted from (api/pagination.go orders on
	// (created_at, id) DESC). It is the 000001 column; EnrolledAt is 000045's and records the same
	// instant for every row this code writes, but the page orders on the former.
	CreatedAt time.Time
}

// Registration is what the gateway knows at enrol time. It carries no id: minting one is the
// registry's job and the reason this type has no field for it.
type Registration struct {
	PoolID          string
	Label           string
	DNS             string
	PublicKeySHA256 string
	OS              string
	Arch            string
	// Capacity is the number of concurrent leases the machine says it can hold. It is RECORDED and
	// nothing enforces it yet — the gateway's `available` channel is unbuffered and counts nothing
	// (§3.6 D13). T4 is where a placement decision may read it.
	Capacity int
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
	// Register mints an id, records the machine, and appends the `issued` enrollment-journal entry
	// in ONE transaction — a certificate the journal does not record is a certificate no revoke can
	// find. Returns ErrUnknownPool for a pool that is not there.
	Register(ctx context.Context, reg Registration) (Runner, error)
	// RecordSeen advances LastSeenAt (and the certificate expiry) for the runner holding dns, and
	// reports whether such a row exists. It is called on connect and on renew — the two moments a
	// machine proves liveness by authenticating with its certificate — and it is deliberately NOT a
	// health check: it writes what just happened, it does not judge what did not.
	RecordSeen(ctx context.Context, dns string, certNotAfter time.Time, at time.Time) (Runner, bool, error)
	Get(ctx context.Context, org, project, id string) (Runner, bool, error)
	List(ctx context.Context, org, project string, window ListWindow) ([]Runner, error)
}
