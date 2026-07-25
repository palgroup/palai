package extensions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/storage"
)

// The Slack admission bridge (E19 T1, spec §36). It is E17 T2's A2A shape one for one — the same three
// parts, for the same reasons:
//
//  1. The REAL Admitter. An inbound Slack event becomes exactly the admission POST /v1/responses takes, so it
//     mints no run identity of its own (§34.1) and inherits every gate that path has: the per-project caps,
//     the durable budget/quota check, the published-revision pin, the atomic idempotency reservation.
//  2. Scope from the AUTHENTICATED side only. a2aScopeFunc reads the tenant from the verified bearer; here
//     there is no bearer, so the tenant is the org/project of the slack_connections row the team id resolved
//     AND WHOSE SECRET SIGNED THIS BODY. No field of the payload participates. That is not a filter over the
//     payload (a filter can be forgotten): the org/project simply never come from it (§2, §38.6).
//  3. Its own idempotency namespace. slackAdmitRoute is deliberately distinct from createRoute and from
//     a2aAdmitRoute, so a Slack event_id can never collide with a native Idempotency-Key or an A2A messageId
//     of the same value.
//
// The RUN TARGET comes from the connection's default_policy JSONB. E19 takes no migration and that column is
// defined as "default run policy for events on this connection" (000035), so reading the pinned revision and
// the owning principal out of it is the column's purpose rather than a workaround.

// slackAdmitRoute is the idempotency route the Slack admission keys on (the a2aAdmitRoute precedent). The key
// under it is team_id + event_id — Slack's own globally-unique event identity, scoped by workspace so two
// workspaces cannot collide even if Slack's uniqueness claim were ever narrower than stated.
const slackAdmitRoute = "/v1/slack/events"

var (
	// ErrSlackNoRunTarget is a connection whose default_policy names no usable run target. Fail-closed: a
	// workspace binding that has not been told WHAT to run, or AS WHOM, admits nothing.
	ErrSlackNoRunTarget = errors.New("extensions: slack connection default_policy names no run target")
	// ErrSlackForeignPrincipal is a default_policy naming a principal outside the connection's own tenant.
	ErrSlackForeignPrincipal = errors.New("extensions: slack connection principal is outside the connection's tenant")
)

// SlackAdmitter wires the Slack Events route to the durable spine. It is the api.SlackEventsAPI production
// implementation.
type SlackAdmitter struct {
	store     *Store
	admitter  api.Admitter
	secrets   SecretResolver
	limits    api.AdmissionLimits
	tolerance time.Duration
	now       func() time.Time
}

// NewSlackAdmitter builds the bridge. secrets redeems a signing_secret_ref to bytes at verification time (the
// inboundSecretResolver sibling) — the bytes are never held, never returned across the api seam, never logged.
func NewSlackAdmitter(store *Store, admitter api.Admitter, secrets SecretResolver, limits api.AdmissionLimits) *SlackAdmitter {
	return &SlackAdmitter{store: store, admitter: admitter, secrets: secrets, limits: limits}
}

// ResolveConnection establishes the tenant for an unauthenticated callback from the untrusted team id. It
// returns only the handle-free projection the route needs; the secret refs stay on this side.
func (a *SlackAdmitter) ResolveConnection(ctx context.Context, teamID, enterpriseID string) (api.SlackConnectionRef, bool, error) {
	conn, found, err := a.store.ResolveSlackConnectionByTeam(ctx, teamID, enterpriseID)
	if err != nil || !found {
		return api.SlackConnectionRef{}, false, err
	}
	return api.SlackConnectionRef{
		ID: conn.ID, Org: conn.Org, Project: conn.Project,
		TeamID: teamID, EnterpriseID: enterpriseID, BotUserID: conn.BotUserID, Disabled: conn.Disabled,
		SigningSecretRef: conn.SigningSecretRef, RunPolicy: conn.DefaultPolicy,
	}, true, nil
}

// VerifySignature redeems the connection's signing_secret_ref and runs the adapter's v0 check over the RAW
// body. The secret bytes exist only inside this call. An unresolvable ref is a verification FAILURE, never a
// pass: a receiver that cannot check a signature must refuse, not accept.
func (a *SlackAdmitter) VerifySignature(_ context.Context, conn api.SlackConnectionRef, timestamp, signature string, body []byte) error {
	if a.secrets == nil || conn.SigningSecretRef == "" {
		return slack.ErrBadSignature
	}
	secret, err := a.secrets(conn.Org, conn.SigningSecretRef)
	if err != nil || len(secret) == 0 {
		// Logged by the caller as a typed reject; the ref name is NOT echoed to the sender (no config oracle).
		return slack.ErrBadSignature
	}
	return slack.VerifySignature(secret, timestamp, signature, body, a.clock(), a.replayWindow())
}

// Admit reserves the source-event dedupe and births the run. Order inside one call:
//
//	run target from default_policy → thread↔session lookup → the REAL admission (which IS the reservation)
//	→ thread↔session claim for a freshly minted session.
//
// The thread claim runs AFTER the admission because the session the thread correlates to is the one the
// admission resolved. A crash in between is self-healing: the redelivery replays the SAME idempotency record
// (no second run) and claims the thread with the replayed session.
func (a *SlackAdmitter) Admit(ctx context.Context, conn api.SlackConnectionRef, ev slack.Event) (api.SlackAdmitOutcome, error) {
	target, err := a.runTarget(ctx, conn)
	if err != nil {
		if errors.Is(err, ErrSlackNoRunTarget) || errors.Is(err, ErrSlackForeignPrincipal) {
			// Terminal by construction: a redelivery cannot fix a connection's configuration.
			return api.SlackAdmitOutcome{Rejected: "the Slack connection has no usable run target"}, nil
		}
		return api.SlackAdmitOutcome{}, err
	}

	// SLK-003: a thread that already has a canonical session CHAINS onto it, so a second message in the
	// thread continues the conversation instead of opening a parallel one. The session id is looked up
	// server-side from (team, channel, thread) — never taken from the payload.
	var requested *string
	existing, _, err := a.store.threadSession(ctx, conn.Org, conn.Project, ev.TeamID, ev.ChannelID, ev.ThreadTS)
	switch {
	case err == nil && existing != "":
		requested = &existing
	case err != nil && !errors.Is(err, ErrSlackThreadSessionNotFound):
		return api.SlackAdmitOutcome{}, err
	}

	scope := middleware.Scope{Organization: conn.Org, Project: conn.Project, Principal: target.principal}
	responseID, runID, sessionID := newID("resp"), newID("run"), newID("ses")
	input := slackRunInput(ev, conn, target)
	create := contracts.ResponseCreateRequest{Input: input, Store: true}
	if target.agentRevisionID != "" {
		rev := target.agentRevisionID
		create.AgentRevisionID = &rev
	}
	hash, err := slackRequestHash(create)
	if err != nil {
		return api.SlackAdmitOutcome{}, err
	}
	body, err := json.Marshal(contracts.Response{
		ID:             contracts.ResponseID(responseID),
		Object:         "response",
		Status:         "queued",
		CreatedAt:      a.clock().UTC().Format(time.RFC3339Nano),
		Output:         []contracts.ContentItem{},
		Usage:          contracts.Usage{},
		SessionID:      contracts.SessionID(sessionID),
		RunID:          contracts.RunID(runID),
		OrganizationID: contracts.OrganizationID(conn.Org),
		ProjectID:      contracts.ProjectID(conn.Project),
	})
	if err != nil {
		return api.SlackAdmitOutcome{}, fmt.Errorf("marshal slack projection: %w", err)
	}
	rawInput, err := json.Marshal(input)
	if err != nil {
		return api.SlackAdmitOutcome{}, fmt.Errorf("marshal slack input: %w", err)
	}

	out, err := a.admitter.AdmitResponse(ctx, api.AdmitRequest{
		Scope: scope,
		// team_id + event_id: the workspace scopes Slack's own event identity, so the reservation is exactly
		// "this source event, once". A redelivery presents the same key and REPLAYS — that is the whole of
		// SLK-001/002, and it commits before the route acks.
		IdempotencyKey:     ev.TeamID + ":" + ev.SourceEventID,
		Method:             "POST",
		Route:              slackAdmitRoute,
		RequestHash:        hash,
		ResponseID:         responseID,
		RunID:              runID,
		SessionID:          sessionID,
		RequestedSessionID: requested,
		Input:              rawInput,
		Body:               body,
		Store:              true,
		AgentRevisionID:    target.agentRevisionID,
		MaxConcurrentRuns:  a.limits.MaxConcurrentRuns,
		MaxQueuedRuns:      a.limits.MaxQueuedRuns,
	})
	if err != nil {
		return api.SlackAdmitOutcome{}, err
	}
	if rejected, retryable := slackAdmitRejection(out); rejected != "" {
		return api.SlackAdmitOutcome{Rejected: rejected, Retryable: retryable}, nil
	}

	// The session the admission settled on (the minted one for a fresh session, the chained one otherwise).
	session := sessionIDFromProjection(out.Body)
	if session == "" {
		session = sessionID
	}
	if requested == nil {
		// First event in this thread: claim the correlation. A concurrent claim collapses at the unique index
		// and the loser reads the winner, so a thread never ends up with two sessions (SLK-003).
		if _, _, err := a.store.CorrelateThreadSession(ctx, conn.Org, conn.Project, conn.ID,
			ev.TeamID, ev.ChannelID, ev.ThreadTS, session); err != nil {
			// The run is already durable and the reservation is committed; failing the ack here would earn a
			// redelivery that REPLAYS onto the same response and can re-attempt the claim.
			return api.SlackAdmitOutcome{}, fmt.Errorf("correlate slack thread session: %w", err)
		}
	}
	return api.SlackAdmitOutcome{ResponseID: out.ResponseID, SessionID: session, Replayed: out.Replayed}, nil
}

// slackRunTarget is what a connection's default_policy says to run and as whom. Both come from the CONNECTION
// row; neither can be influenced by a payload.
type slackRunTarget struct {
	agentRevisionID string
	principal       string
}

// runTarget decodes and validates default_policy. Fail-closed on every branch: no principal, no revision, or
// a principal belonging to another tenant all refuse the admission rather than guess a default.
func (a *SlackAdmitter) runTarget(ctx context.Context, conn api.SlackConnectionRef) (slackRunTarget, error) {
	var policy struct {
		AgentRevisionID string `json:"agent_revision_id"`
		PrincipalID     string `json:"principal_id"`
	}
	if len(conn.RunPolicy) > 0 {
		if err := json.Unmarshal(conn.RunPolicy, &policy); err != nil {
			return slackRunTarget{}, fmt.Errorf("%w: default_policy is not an object", ErrSlackNoRunTarget)
		}
	}
	if policy.PrincipalID == "" {
		// A Slack user is NOT a principal and must never become one: SLK-004's event half is that an unmapped
		// clicker is a CONSTRAINED integration actor. The run's identity therefore has to be configured on the
		// connection, or the connection cannot admit at all.
		return slackRunTarget{}, fmt.Errorf("%w: default_policy.principal_id is required", ErrSlackNoRunTarget)
	}
	if policy.AgentRevisionID == "" {
		return slackRunTarget{}, fmt.Errorf("%w: default_policy.agent_revision_id is required", ErrSlackNoRunTarget)
	}
	// The principal must live in the connection's OWN tenant — see SlackRunPrincipalInScope.
	switch err := a.store.pool.QueryRow(storage.WithSystemScope(ctx), storage.Query("SlackRunPrincipalInScope"),
		policy.PrincipalID, conn.Org, conn.Project).Scan(new(int)); {
	case errors.Is(err, pgx.ErrNoRows):
		return slackRunTarget{}, ErrSlackForeignPrincipal
	case err != nil:
		return slackRunTarget{}, fmt.Errorf("resolve slack run principal: %w", err)
	}
	return slackRunTarget{agentRevisionID: policy.AgentRevisionID, principal: policy.PrincipalID}, nil
}

// slackRunInput is the run's input: the canonical Slack identity plus the inner event VERBATIM. The inner
// event stays opaque here exactly as the adapter left it — turning a Slack message into a prompt is the
// pinned agent revision's job, not the transport's.
//
// actor_mapped records SLK-004's event half: whether the Slack user who caused this event is on the
// connection's approver allow-list. It is a FACT ABOUT the actor carried as data — an unmapped user still
// gets a run, that run simply belongs to the connection's principal and its Slack actor authorizes nothing.
func slackRunInput(ev slack.Event, conn api.SlackConnectionRef, target slackRunTarget) map[string]any {
	return map[string]any{
		"source":        slack.Source,
		"connection_id": conn.ID,
		"team_id":       ev.TeamID,
		"event_id":      ev.SourceEventID,
		"channel_id":    ev.ChannelID,
		"thread_ts":     ev.ThreadTS,
		"slack_user_id": ev.UserID,
		"kind":          string(ev.Kind),
		"principal_id":  target.principal,
		"event":         json.RawMessage(ev.Data),
	}
}

// NOTE, and it is the kind of detail that silently breaks a dedupe: nothing about the DELIVERY ATTEMPT may
// appear above. Putting ev.Retry in here would make a redelivery hash differently from the original, and the
// idempotency reservation would then report a CONFLICT rather than a replay — Slack's retry would be refused
// as "the same key with a different request" instead of collapsing onto the one run. The input is a pure
// function of the EVENT; the retry hint stays where it belongs, on the route's ledger.

// slackRequestHash is the §20.9 step-2 canonical request hash (api.canonicalRequestHash's construction). It
// must be a pure function of the event, or a redelivery would hash differently and the reservation would
// report an idempotency CONFLICT instead of a replay — the exact opposite of SLK-002.
func slackRequestHash(req contracts.ResponseCreateRequest) (string, error) {
	canonical, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// slackAdmitRejection names a typed admission refusal and says whether a Slack redelivery could ever change
// it. The split is what decides the D1 no-retry header upstream: load empties, configuration does not.
func slackAdmitRejection(out api.AdmitResult) (reason string, retryable bool) {
	switch {
	case out.ConcurrencyLimited:
		return "too many concurrent runs", true
	case out.QueueDepthExceeded:
		return "the run queue is full", true
	case out.ActiveRunConflict:
		// The thread's session already has a live run. Retryable because the run ENDS — Slack's +1min/+5min
		// attempts may well land after it. CEILING: turning a mid-run Slack message into a queued send_message
		// on the live run (the §63.3 journey's named_session behaviour) is NOT wired by T1.
		return "the session already has an active run", true
	case out.LimitExceeded != nil:
		return "a durable budget or quota is exhausted", true
	case out.SessionNotFound:
		return "the correlated session no longer exists", false
	case out.SessionConflict:
		return "the correlated session is not active", false
	case out.PinnedRevisionNotFound:
		return "the connection pins an unknown agent revision", false
	case out.PinnedRevisionNotPublished:
		return "the connection pins a draft agent revision", false
	case out.RepositoryBindingNotFound:
		return "no such repository binding", false
	case out.Conflict:
		return "the source event id was reused with a different request", false
	case out.Purged:
		return "the idempotent result has been reaped", false
	}
	return "", false
}

// sessionIDFromProjection reads the session the admission settled on out of the stored projection.
func sessionIDFromProjection(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var proj struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(body, &proj); err != nil {
		return ""
	}
	return proj.SessionID
}

func (a *SlackAdmitter) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

func (a *SlackAdmitter) replayWindow() time.Duration {
	if a.tolerance > 0 {
		return a.tolerance
	}
	return slack.DefaultTolerance
}
