package fleet

// THE POOL ENROLMENT KEY (E24 T3): the credential a machine presents to enrol into ONE pool.
//
// WHAT THIS ADDS IS NOT REUSABILITY, and getting that the wrong way round would make the whole task
// look like a rewrite of something that already works. The tree's one production enrolment credential
// is ALREADY reusable and says so at length — `FileEnrollmentTokens`' heading reads "WHY THIS IS NOT
// ONE-USE, AND WHAT REPLACED THAT" (execution/local_credentials.go), because a strictly one-use token
// left a runner whose certificate had expired with no way back at all. What that credential has never
// had is SCOPE (it admits into whatever the default pool is), a HASH (it is a plaintext line in a
// file), an EXPIRY (it never stops working), a REVOCATION (deleting the file also closes the only
// recovery path) and a RECORD (nothing says which credential minted which certificate). This file is
// those five, and the file token stays exactly where it is as the second link in the chain.
//
// THE HONEST CEILING, stated where it is implemented rather than only in a plan: whoever holds a pool
// key can enrol a machine of their own into that pool and be offered real work, because a pool key
// authorises enrolment and nothing on this wire attests what the machine IS (FLT-P2), and strict
// enrolment — the waiting room that would put a human in front of it — is T6's and is OFF by default.
// The defences are key secrecy and revocation speed. There are exactly two, and this comment is not
// going to imply a third.
//
// THE SECOND CEILING, and it is a MEASURED CORRECTION to what §T3 asked for: the number of concurrent
// identities one key can mint is NOT capped, and it deliberately is not rate-limited either. See
// TouchRunnerPoolKey in storage/queries/runners.sql — the file token's one-certificate-per-lifetime
// rule exists because ONE runner holds that token, while a pool key is a FLEET credential by
// construction, so the same rule would make a ten-machine pool unenrollable. last_used_at makes the
// redemption fact survive a restart (which the in-memory map behind minInterval does not, §3.6 D6);
// that is an observation, not a quota.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/storage"
)

// ErrUnknownPoolKey is returned when a presented value matches no key row. It is the ONE error that
// means "not mine": the gateway's credential chain falls through to the file bootstrap token on this
// and on nothing else, because every other error here is a key that WAS recognised and refused, and
// falling through on those would let a revoked key be re-tried against a different credential.
var ErrUnknownPoolKey = errors.New("fleet: unknown runner pool key")

// ErrPoolKeyRevoked is returned when the presented key has been revoked. The machines that key already
// admitted are untouched — see Revoke.
var ErrPoolKeyRevoked = errors.New("fleet: runner pool key is revoked")

// ErrPoolKeyExpired is returned when the presented key is past its expiry. FileEnrollmentTokens has no
// expiry at all, which is the gap this closes: a credential that never stops working is a credential
// nobody remembers to withdraw.
var ErrPoolKeyExpired = errors.New("fleet: runner pool key has expired")

// ErrPoolScopeMismatch is returned when the machine DECLARES a pool the presented key does not admit
// into. The key is authoritative and is never widened by the declaration; the declaration exists so
// that pasting the wrong pool's key onto a machine is a refusal at the door instead of a machine
// quietly joining another fleet — the mistake posture comparison (T2) cannot catch when two pools share
// a posture.
var ErrPoolScopeMismatch = errors.New("fleet: the presented key does not admit into the declared pool")

// PoolGrant is what a redeemed key authorises: one enrolment, into one pool, recorded against one key.
type PoolGrant struct {
	PoolID string
	// KeyID is written to runners.enrolled_via_key_id and to the journal entry. It is the fact §3.6 D5
	// names as missing, and it is the only thing that makes a revocation targetable rather than total.
	KeyID string
}

// MintedKey is a freshly minted key. Value is populated EXACTLY ONCE, here, and is never read back from
// anywhere: only the digest is stored.
type MintedKey struct {
	ID        string
	PoolID    string
	Prefix    string
	Value     string
	CreatedAt time.Time
	ExpiresAt *time.Time
}

// PoolKey is a key's operator-facing metadata: enough to tell two keys apart and to see the state of
// each, and not enough to present one.
type PoolKey struct {
	ID     string
	PoolID string
	// Prefix is the value's first 8 characters. A prefix of a credential is not a credential; it is the
	// same bargain `palai apikey list` already makes.
	Prefix     string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
	LastUsedAt *time.Time
}

// EnrolledRunner is one machine a revoked key admitted, as the operator is shown it.
type EnrolledRunner struct {
	ID         string
	Label      string
	DNS        string
	State      string
	PoolID     string
	EnrolledAt time.Time
	LastSeenAt *time.Time
}

// Revocation is the answer to a revoke: the key's new state, and THE MACHINES IT ALREADY ADMITTED —
// none of which was stopped. Returning them is not a convenience: without the list an operator reads
// "revoked" as "removed" and believes one call decommissioned a fleet.
type Revocation struct {
	Key             PoolKey
	EnrolledRunners []EnrolledRunner
}

// PoolEnrollmentKeys is the second implementation of execution.EnrollmentTokens, beside
// FileEnrollmentTokens rather than in place of it. It is a separate type from Store because it is a
// separate concern — Store is the machine INVENTORY, this is credential verification — and because the
// gateway wires them separately: a deployment can have a registry and no keys (every deployment before
// this task).
type PoolEnrollmentKeys struct {
	pool  *pgxpool.Pool
	newID func(prefix string) string
	now   func() time.Time
}

// NewPoolEnrollmentKeys binds the durable spine's pool. newID mints row ids (pass middleware.NewID in
// production); now defaults to time.Now.
func NewPoolEnrollmentKeys(pool *pgxpool.Pool, newID func(prefix string) string, now func() time.Time) *PoolEnrollmentKeys {
	if now == nil {
		now = time.Now
	}
	return &PoolEnrollmentKeys{pool: pool, newID: newID, now: now}
}

// keyPrefixLength is how much of a key value is published so an operator can tell two apart in a list.
const keyPrefixLength = 8

// hashKey is the stored verifier. A pool key is a 192-bit random value, so a fast digest is the
// standard verifier (coordinator.HashAPIKey takes the same position for the same reason), not a
// password KDF.
func hashKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// newKeyValue mints the value. The `rpk_` prefix is load-bearing rather than decorative: an API key is
// `sk_`, so a pool key pasted into an Authorization header against /v1 is recognisable as the wrong
// kind of credential by eye, and one pasted into a runner's config is likewise.
func newKeyValue() (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate pool key: %w", err)
	}
	return "rpk_" + hex.EncodeToString(raw[:]), nil
}

// Mint creates a key for one pool inside the caller's verified tenant and returns its value ONCE. The
// pool is re-asserted against the tenant in SQL, so a caller cannot mint a credential into another
// tenant's fleet by naming its pool id. A pool that is not the caller's (or does not exist) returns
// found=false as ErrUnknownPool, which the surface renders as a 404.
func (k *PoolEnrollmentKeys) Mint(ctx context.Context, org, project, poolID string, expiresAt *time.Time) (MintedKey, error) {
	if org == "" || poolID == "" {
		return MintedKey{}, ErrUnknownPool
	}
	value, err := newKeyValue()
	if err != nil {
		return MintedKey{}, err
	}
	out := MintedKey{Value: value}
	ctx = storage.WithTenant(ctx, org, project)
	err = k.pool.QueryRow(ctx, storage.Query("InsertRunnerPoolKey"),
		k.mintID("rpk"), project, poolID, hashKey(value), value[:keyPrefixLength], expiresAt,
	).Scan(&out.ID, &out.PoolID, &out.Prefix, &out.CreatedAt, &out.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return MintedKey{}, ErrUnknownPool
	}
	if err != nil {
		// The value is in scope in this function and must never reach an error string: %w carries the
		// driver's message, which names parameters by NUMBER and never by value.
		return MintedKey{}, fmt.Errorf("insert runner pool key: %w", err)
	}
	return out, nil
}

// List returns this tenant's keys, newest first, as metadata. poolID narrows to one pool; empty lists
// every pool's. There is no page window: an operator has a handful of pools and a handful of keys, and
// a cursor nobody needs is a cursor to keep in step with nothing.
func (k *PoolEnrollmentKeys) List(ctx context.Context, org, project, poolID string) ([]PoolKey, error) {
	ctx = storage.WithTenant(ctx, org, project)
	rows, err := k.pool.Query(ctx, storage.Query("ListRunnerPoolKeys"), project, poolID)
	if err != nil {
		return nil, fmt.Errorf("list runner pool keys: %w", err)
	}
	defer rows.Close()
	out := []PoolKey{}
	for rows.Next() {
		var row PoolKey
		if err := rows.Scan(&row.ID, &row.PoolID, &row.Prefix, &row.CreatedAt, &row.ExpiresAt,
			&row.RevokedAt, &row.LastUsedAt); err != nil {
			return nil, fmt.Errorf("scan runner pool key: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate runner pool keys: %w", err)
	}
	return out, nil
}

// Revoke closes a key and reports the machines it already admitted — none of which is stopped. Both
// halves run in ONE transaction so the list an operator is shown is the list as of the revocation: a
// machine that enrols between the UPDATE and the SELECT would otherwise be invisible in the answer
// while being perfectly visible in the fleet.
//
// Idempotent: the first revoked_at is kept (the api_keys precedent). An unknown or foreign key id is
// ErrUnknownPoolKey, so a caller learns nothing about another tenant's ids.
func (k *PoolEnrollmentKeys) Revoke(ctx context.Context, org, project, keyID string) (Revocation, error) {
	ctx = storage.WithTenant(ctx, org, project)
	tx, err := k.pool.Begin(ctx)
	if err != nil {
		return Revocation{}, fmt.Errorf("begin revoke runner pool key: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var out Revocation
	err = tx.QueryRow(ctx, storage.Query("RevokeRunnerPoolKey"), keyID, project, k.now()).
		Scan(&out.Key.ID, &out.Key.PoolID, &out.Key.Prefix, &out.Key.CreatedAt, &out.Key.ExpiresAt,
			&out.Key.RevokedAt, &out.Key.LastUsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Revocation{}, ErrUnknownPoolKey
	}
	if err != nil {
		return Revocation{}, fmt.Errorf("revoke runner pool key: %w", err)
	}
	rows, err := tx.Query(ctx, storage.Query("ListRunnersEnrolledViaKey"), keyID, project)
	if err != nil {
		return Revocation{}, fmt.Errorf("list machines enrolled via key: %w", err)
	}
	out.EnrolledRunners = []EnrolledRunner{}
	for rows.Next() {
		var row EnrolledRunner
		if err := rows.Scan(&row.ID, &row.Label, &row.DNS, &row.State, &row.PoolID, &row.EnrolledAt, &row.LastSeenAt); err != nil {
			rows.Close()
			return Revocation{}, fmt.Errorf("scan machine enrolled via key: %w", err)
		}
		out.EnrolledRunners = append(out.EnrolledRunners, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Revocation{}, fmt.Errorf("iterate machines enrolled via key: %w", err)
	}
	// Named here rather than returned through a helper: `return helper(tx, …)` under a deferred
	// Rollback is how two writes were silently dropped in this tree.
	if err := tx.Commit(ctx); err != nil {
		return Revocation{}, fmt.Errorf("commit revoke runner pool key: %w", err)
	}
	return out, nil
}

// RedeemPoolKey resolves a PRESENTED key into the grant it authorises, and journals every refusal it is
// able to attribute to a key row.
//
// It runs SYSTEM-SCOPED and keyed by the digest, for the reason VerifyAPIKey does: this read is what
// establishes the tenant, so there is no tenant to publish yet, and the tenant it goes on to use comes
// off the KEY ROW rather than off anything on the wire (the enrolment wire carries none — §3.6 D8).
//
// runnerID is the server-minted id the caller will register under; it is the subject a refusal is
// journalled against. declaredPool is what the MACHINE said, and it never widens the grant.
func (k *PoolEnrollmentKeys) RedeemPoolKey(ctx context.Context, presented, runnerID, declaredPool string) (PoolGrant, error) {
	if presented == "" {
		return PoolGrant{}, ErrUnknownPoolKey
	}
	digest := hashKey(presented)
	ctx = storage.WithSystemScope(ctx)
	tx, err := k.pool.Begin(ctx)
	if err != nil {
		return PoolGrant{}, fmt.Errorf("begin redeem runner pool key: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var row struct {
		id, org, project, poolID, storedDigest, posture string
		revokedAt, expiresAt                            *time.Time
		strict                                          bool
	}
	err = tx.QueryRow(ctx, storage.Query("ResolveRunnerPoolKey"), digest).Scan(
		&row.id, &row.org, &row.project, &row.poolID, &row.storedDigest,
		&row.revokedAt, &row.expiresAt, &row.posture, &row.strict)
	if errors.Is(err, pgx.ErrNoRows) {
		return PoolGrant{}, ErrUnknownPoolKey
	}
	if err != nil {
		return PoolGrant{}, fmt.Errorf("resolve runner pool key: %w", err)
	}
	// The digest was already the lookup key, so this comparison can only fail if the row that came back
	// is not the row the digest names. It is here for the reason api/tool_callbacks.go:98 does the same
	// after its own keyed read: a credential comparison in this tree is constant-time, without a
	// per-site argument about whether this particular one leaks anything.
	if subtle.ConstantTimeCompare([]byte(digest), []byte(row.storedDigest)) != 1 {
		return PoolGrant{}, ErrUnknownPoolKey
	}

	refuse := func(reason string, detail map[string]string, sentinel error) (PoolGrant, error) {
		if err := k.journalRefusal(ctx, tx, row.org, row.project, runnerID, row.poolID, row.id, reason, detail); err != nil {
			return PoolGrant{}, err
		}
		// Commit the refusal: it is the only thing this transaction produced, and rolling it back would
		// make the refusal invisible — which is the whole reason an operator has a journal to read.
		if err := tx.Commit(ctx); err != nil {
			return PoolGrant{}, fmt.Errorf("commit pool key refusal: %w", err)
		}
		return PoolGrant{}, sentinel
	}
	switch {
	case row.revokedAt != nil:
		return refuse("key_revoked", nil, ErrPoolKeyRevoked)
	case row.expiresAt != nil && !row.expiresAt.After(k.now()):
		return refuse("key_expired", nil, ErrPoolKeyExpired)
	case declaredPool != "" && declaredPool != row.poolID:
		return refuse("pool_scope_mismatch",
			map[string]string{"declared_pool": declaredPool, "key_pool": row.poolID}, ErrPoolScopeMismatch)
	}

	if _, err := tx.Exec(ctx, storage.Query("TouchRunnerPoolKey"), row.id, k.now()); err != nil {
		return PoolGrant{}, fmt.Errorf("record pool key redemption: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return PoolGrant{}, fmt.Errorf("commit pool key redemption: %w", err)
	}
	return PoolGrant{PoolID: row.poolID, KeyID: row.id}, nil
}

// journalRefusal appends the `refused` entry for a key that WAS recognised. It is skipped when there is
// no runner id to attribute it to (runner_enrollments is keyed by runner), which is the Consume path
// below — a journal row about no machine is not a record of anything.
func (k *PoolEnrollmentKeys) journalRefusal(ctx context.Context, tx pgx.Tx, org, project, runnerID, poolID, keyID, reason string, extra map[string]string) error {
	if runnerID == "" {
		return nil
	}
	detail := map[string]string{"reason": reason}
	for key, value := range extra {
		detail[key] = value
	}
	// There is no field here a credential could be put in, and that is how §2's "the journal writes
	// key_id and never a key VALUE" stays true as this grows: the payload is built from a fixed set of
	// keys, not from the caller's input.
	encoded, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("encode pool key refusal detail: %w", err)
	}
	if _, err := tx.Exec(ctx, storage.Query("AppendRunnerEnrollment"),
		k.mintID("renr"), org, project, runnerID, poolID, keyID, "refused", encoded); err != nil {
		return fmt.Errorf("append pool key refusal: %w", err)
	}
	return nil
}

// Consume implements execution.EnrollmentTokens, which is what makes this type substitutable wherever
// FileEnrollmentTokens is — enforced by the compiler rather than promised in a comment, because the
// gateway's own seam (execution.PoolEnrollment) embeds that interface.
//
// It answers the pool-blind question — "is this a live credential" — and DISCARDS the binding, which is
// why the gateway calls RedeemPoolKey instead: the pool a key admits into is the whole point, and a
// caller that dropped it would send every machine to the default pool. It journals nothing, because a
// refusal with no machine to attribute it to is not a record.
func (k *PoolEnrollmentKeys) Consume(token string) error {
	_, err := k.RedeemPoolKey(context.Background(), token, "", "")
	return err
}

func (k *PoolEnrollmentKeys) mintID(prefix string) string {
	if k.newID != nil {
		return k.newID(prefix)
	}
	return fmt.Sprintf("%s_%d", prefix, k.now().UnixNano())
}
