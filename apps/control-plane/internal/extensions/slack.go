package extensions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/storage"
)

// The Slack connection registry + thread↔session correlation store (spec §36, E17 Task 1, SLK-001..008). A
// connection is an admin-registered workspace binding whose signing secret + bot token are secret_ref
// HANDLES only — a credential can never enter a row (DisallowUnknownFields rejects an inline value field),
// so it also never reaches a log or an evidence bundle. This is the tenant-scoped, RLS-forced store the
// PURE adapters/integrations/slack package's verify + mapping are wired against control-plane-side; the
// adapter itself holds no database, exactly like the webhook seam.

var (
	// ErrInvalidSlackConfig is a missing team id or signing-secret ref, or a malformed body.
	ErrInvalidSlackConfig = errors.New("extensions: slack connection config is invalid")
	// ErrSlackConnectionExists is a workspace already bound in this project (team_id + enterprise_id).
	ErrSlackConnectionExists = errors.New("extensions: slack connection already exists for this workspace")
	// ErrSlackWorkspaceBoundElsewhere is a workspace already bound in a DIFFERENT org/project. It carries no
	// hint of WHICH tenant holds it: the registering admin has no business learning that another customer
	// exists, let alone which one.
	ErrSlackWorkspaceBoundElsewhere = errors.New("extensions: slack workspace is already bound elsewhere")
	// ErrSlackConnectionAmbiguous is a team id resolving to MORE THAN ONE connection row. It can only happen
	// when two tenants both hold a binding for one workspace, which CreateSlackConnection refuses — but that
	// check is a check-then-insert, and a deployment upgraded from before it may already hold such rows. The
	// receiver must refuse rather than guess a tenant, and refuse REPAIRABLY (no retry suppression), because
	// an operator can fix a 503 while a suppressed retry is simply a lost event.
	ErrSlackConnectionAmbiguous = errors.New("extensions: slack team id resolves to more than one connection")
	// ErrSlackConnectionNotFound is a get/resolve for a connection absent from scope.
	ErrSlackConnectionNotFound = errors.New("extensions: slack connection not found in scope")
	// ErrSlackThreadSessionNotFound is a thread-session read for a (team, channel, thread) with no correlated
	// session yet — the connection exists, the thread row does not.
	ErrSlackThreadSessionNotFound = errors.New("extensions: slack thread session not found in scope")
)

// SlackConnection is a registered workspace binding's committed shape. The refs are handles, never values.
type SlackConnection struct {
	ID               string
	TeamID           string
	EnterpriseID     string
	BotUserID        string
	SigningSecretRef string
	BotTokenRef      string
	AppTokenRef      string
	Scopes           string
	Disabled         bool
}

// SlackConnectionInput is the strict-decoded create body. A field outside this struct — including a raw
// `signing_secret` / `bot_token` VALUE — is rejected, so a credential can only enter as a *_ref handle.
type SlackConnectionInput struct {
	TeamID           string         `json:"team_id"`
	EnterpriseID     string         `json:"enterprise_id"`
	BotUserID        string         `json:"bot_user_id"`
	SigningSecretRef string         `json:"signing_secret_ref"`
	BotTokenRef      string         `json:"bot_token_ref"`
	AppTokenRef      string         `json:"app_token_ref"` // Socket Mode WS app-token (xapp-) handle; ref only, never a raw token
	Scopes           string         `json:"scopes"`
	AllowedChannels  []string       `json:"allowed_channels"`
	AllowedUsers     []string       `json:"allowed_users"`
	DefaultPolicy    map[string]any `json:"default_policy"`
}

// CreateSlackConnection registers a workspace binding. It is an admin action — never reachable from a tool
// the model can call. A team already bound in the project is a typed collision; a team already bound in
// ANOTHER tenant is refused outright (see the cross-tenant check below).
func (s *Store) CreateSlackConnection(ctx context.Context, project string, raw []byte) (SlackConnection, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	in, err := decodeSlackInput(raw)
	if err != nil {
		return SlackConnection{}, err
	}
	if in.TeamID == "" {
		return SlackConnection{}, fmt.Errorf("%w: team_id is required", ErrInvalidSlackConfig)
	}
	if in.SigningSecretRef == "" {
		return SlackConnection{}, fmt.Errorf("%w: signing_secret_ref is required (the v0 verify resolves it)", ErrInvalidSlackConfig)
	}
	// The workspace may not already belong to a DIFFERENT tenant. 000035's uniqueness is per-tenant, so the
	// database would happily accept the row and the unauthenticated resolve — keyed by team_id alone — would
	// then have two candidates for one Slack workspace. See SlackWorkspaceBoundElsewhere for the whole shape
	// of the hole this closes.
	//
	// ponytail: check-then-insert. Two concurrent registrations in different tenants can both pass it, and a
	// deployment upgraded from before this check may already hold the rows — which is precisely why
	// ResolveSlackConnectionByTeam refuses an ambiguous resolve rather than trusting this guard alone.
	// Closing the race outright needs a GLOBAL unique index on (team_id, enterprise_id), i.e. a migration,
	// and E19 takes none.
	var otherID string
	switch err := s.pool.QueryRow(storage.WithSystemScope(ctx),
		storage.Query("SlackWorkspaceBoundElsewhere"), in.TeamID, in.EnterpriseID, project).
		Scan(&otherID); {
	case err == nil:
		// The detail names the workspace, never the tenant holding it: the caller must not learn that another
		// customer exists. The id is logged control-plane-side for the operator who has to resolve it.
		log.Printf("slack: refused a registration for a workspace already bound by connection %s", otherID)
		return SlackConnection{}, ErrSlackWorkspaceBoundElsewhere
	case !errors.Is(err, pgx.ErrNoRows):
		return SlackConnection{}, fmt.Errorf("check slack workspace binding: %w", err)
	}
	id := newID("slkc")
	if _, err := s.pool.Exec(ctx, storage.Query("InsertSlackConnection"),
		id, project, in.TeamID, in.EnterpriseID, in.BotUserID,
		in.SigningSecretRef, in.BotTokenRef, in.AppTokenRef, in.Scopes,
		marshalJSON(orEmptyList(in.AllowedChannels)), marshalJSON(orEmptyList(in.AllowedUsers)),
		marshalJSON(orEmptyObject(in.DefaultPolicy))); err != nil {
		if isUniqueViolation(err) {
			return SlackConnection{}, ErrSlackConnectionExists
		}
		return SlackConnection{}, fmt.Errorf("insert slack connection: %w", err)
	}
	return SlackConnection{
		ID: id, TeamID: in.TeamID, EnterpriseID: in.EnterpriseID, BotUserID: in.BotUserID,
		SigningSecretRef: in.SigningSecretRef, BotTokenRef: in.BotTokenRef, AppTokenRef: in.AppTokenRef, Scopes: in.Scopes,
	}, nil
}

// GetSlackConnection reads a connection's metadata within scope (the refs are handles, safe to return).
func (s *Store) GetSlackConnection(ctx context.Context, project, id string) (SlackConnection, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	c := SlackConnection{ID: id}
	err := s.pool.QueryRow(ctx, storage.Query("GetSlackConnection"), id, project).
		Scan(&c.ID, &c.TeamID, &c.EnterpriseID, &c.BotUserID, &c.SigningSecretRef, &c.BotTokenRef, &c.AppTokenRef, &c.Scopes, &c.Disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return SlackConnection{}, ErrSlackConnectionNotFound
	}
	if err != nil {
		return SlackConnection{}, fmt.Errorf("read slack connection: %w", err)
	}
	return c, nil
}

// SlackAuthorizationPolicy is a connection's two scoping allow-lists (§36.2, SLK-004): which Slack users may
// authorize a high-risk operation, and which channels the integration acts in at all. A user absent from
// AllowedUsers is a CONSTRAINED integration actor — its click carries an identity but authorizes nothing.
//
// THE EMPTINESS OF THE TWO LISTS MEANS OPPOSITE THINGS, and that asymmetry is deliberate rather than an
// accident of history. It is a trap unless a reader meets the reason here, so:
//
//	AllowedUsers    empty ⇒ NOBODY may approve   (deny by default)
//	AllowedChannels empty ⇒ EVERY channel        (no restriction)
//
// The lists sit in front of different boundaries. AllowedChannels NARROWS a gate that already exists: Slack
// delivers events only from conversations the bot was invited to, so an unconfigured connection is already
// scoped by whoever did the inviting, and reading empty as "nowhere" would make every freshly registered
// connection silently inert. AllowedUsers has nothing behind it at all — it is the only thing between "any
// member of the workspace" and authorizing a privileged operation, so its unconfigured state must be deny.
// 000035's column comment ("empty = no channel restriction") already committed to this reading.
//
// Both are enforced. Neither is advisory: see ApproverAuthorized and ChannelAllowed for the callers.
type SlackAuthorizationPolicy struct {
	AllowedChannels []string
	AllowedUsers    []string
}

// ApproverAuthorized reports whether a Slack user may authorize a decision in this connection. This is the
// control-plane half of SLK-004: the adapter's ApprovalIntent carries WHO clicked and in WHICH workspace, and
// this decides. Deny-by-default — an empty allow-list authorizes nobody.
func (p SlackAuthorizationPolicy) ApproverAuthorized(userID string) bool {
	if userID == "" {
		return false
	}
	return slices.Contains(p.AllowedUsers, userID)
}

// ChannelAllowed reports whether the integration may act in a conversation. An EMPTY list means every channel
// — the opposite of ApproverAuthorized's emptiness, deliberately, for the reason documented at the type. An
// unknown channel id ("") against a NON-empty list is refused: a scope that cannot be checked is not met.
//
// isDM EXEMPTS a direct message from the list, and THIS IS A WIDENING (E20 T2). Naming it as one:
//
//   - WHAT IT OPENS. Before it, a connection with a non-empty allowed_channels refused EVERY DM, so the agent
//     panel — whose conversation is literally a DM (message.im) — died silently on any install that had ever
//     narrowed its channel scope. After it, a workspace member who can open a DM with the app can start a run
//     in that DM even though no channel of theirs is listed.
//   - WHY IT IS DEFENSIBLE. A DM's scope is Slack's OWN invitation model: the app can only be DM'd by a member
//     of the workspace it was installed into, and the conversation has exactly two parties. allowed_channels
//     narrows a gate that already exists (which channels the bot was invited to); for a DM that prior gate is
//     workspace membership, not an invitation anyone can widen. This is the same reasoning that makes an EMPTY
//     allowed_channels mean "every channel" rather than "none".
//   - WHAT IT DOES NOT OPEN, and none of these is a promise — each is structural. default_policy is still the
//     ONLY run target (SlackAdmitter.runTarget reads it off the connection row). AllowedUsers is still the ONLY
//     approval gate, and it is still deny-by-default. A DM run carries NO extra authority: the Slack user id
//     stays out of the prompt entirely (slackRunInput) and never becomes a principal (SLK-004).
//
// The parameter is EXPLICIT rather than derived in here, because the two callers have genuinely different
// evidence and a function that guessed would hide that:
//
//	SlackAdmitter.Admit   — passes slack.Event.IsDM(), i.e. Slack's own `channel_type == "im"` (authoritative).
//	SlackAdmitter.Decide  — passes FALSE, because it cannot know. A block_actions payload carries `channel`
//	                        with `{id, name}` and NO channel_type anywhere
//	                        (https://docs.slack.dev/reference/interaction-payloads/block_actions-payload/,
//	                        checked 2026-07-27). A scope that cannot be checked is not met, so a click inside a
//	                        DM is still governed by the list. CONSEQUENCE, stated rather than discovered later:
//	                        under a NON-EMPTY allowed_channels a DM run's approval buttons are refused; the
//	                        operator's escape hatch is to list that DM's channel id, and the durable fix is to
//	                        record the conversation's DM-ness on the correlation row — a migration, and not one
//	                        this task takes.
//
// Decide is not redundant with Admit for the ordinary case either: an allow-list NARROWED after a thread was
// correlated must take that thread's in-flight buttons with it.
func (p SlackAuthorizationPolicy) ChannelAllowed(channelID string, isDM bool) bool {
	if len(p.AllowedChannels) == 0 {
		return true
	}
	if isDM {
		return true
	}
	return slices.Contains(p.AllowedChannels, channelID)
}

// SlackAuthorizationPolicyFor reads the connection's two allow-lists within scope, so a click can be refused
// BEFORE any command is enqueued when the clicking user is not a mapped approver (SLK-004), and so an event or
// a click from outside the configured channel scope is refused before anything is written at all. It reads
// only the two lists — never the secret-ref handles — so the enforcement path never touches credential
// metadata it has no use for.
func (s *Store) SlackAuthorizationPolicyFor(ctx context.Context, project, id string) (SlackAuthorizationPolicy, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	var channels, users []byte
	err := s.pool.QueryRow(ctx, storage.Query("GetSlackAuthorizationPolicy"), id, project).Scan(&channels, &users)
	if errors.Is(err, pgx.ErrNoRows) {
		return SlackAuthorizationPolicy{}, ErrSlackConnectionNotFound
	}
	if err != nil {
		return SlackAuthorizationPolicy{}, fmt.Errorf("read slack authorization policy: %w", err)
	}
	var policy SlackAuthorizationPolicy
	if err := json.Unmarshal(channels, &policy.AllowedChannels); err != nil {
		return SlackAuthorizationPolicy{}, fmt.Errorf("decode allowed_channels: %w", err)
	}
	if err := json.Unmarshal(users, &policy.AllowedUsers); err != nil {
		return SlackAuthorizationPolicy{}, fmt.Errorf("decode allowed_users: %w", err)
	}
	return policy, nil
}

// ResolvedSlackConnection is what the UNAUTHENTICATED inbound path learns from a Slack team id before it has
// a tenant: the org/project it belongs to and the secret_ref handles the caller verifies + replies under.
type ResolvedSlackConnection struct {
	ID               string
	Org              string
	Project          string
	SigningSecretRef string
	BotTokenRef      string
	AppTokenRef      string
	BotUserID        string
	Disabled         bool
	// DefaultPolicy is the connection's default run policy (the 000035 JSONB). It carries the RUN TARGET the
	// E19 T1 admission bridge pins — see slackRunTarget — and nothing a Slack payload can influence.
	DefaultPolicy []byte
}

// ResolveSlackConnectionByTeam establishes the tenant for a signed inbound Slack callback, keyed by the
// team + enterprise id the callback carries (the resolveInboundTrigger idiom). It runs SYSTEM-scoped because
// there is no tenant yet; the caller must still present a valid v0 signature over the returned
// signing_secret_ref before anything is written — the signature is the auth, not this lookup.
// It is read with Query, not QueryRow, and the SQL asks for TWO rows: the predicate is not unique (the index
// is per-tenant) and pgx's QueryRow calls rows.Next() exactly once and closes — a second row would be
// silently discarded and the winner could flip between requests. A second row is therefore an explicit,
// typed refusal, never a coin toss between two tenants.
func (s *Store) ResolveSlackConnectionByTeam(ctx context.Context, teamID, enterpriseID string) (ResolvedSlackConnection, bool, error) {
	ctx = storage.WithSystemScope(ctx)
	rows, err := s.pool.Query(ctx, storage.Query("ResolveSlackConnectionByTeam"), teamID, enterpriseID)
	if err != nil {
		return ResolvedSlackConnection{}, false, fmt.Errorf("resolve slack connection: %w", err)
	}
	defer rows.Close()

	var resolved []ResolvedSlackConnection
	for rows.Next() {
		var r ResolvedSlackConnection
		if err := rows.Scan(&r.ID, &r.Org, &r.Project, &r.SigningSecretRef, &r.BotTokenRef, &r.AppTokenRef,
			&r.BotUserID, &r.Disabled, &r.DefaultPolicy); err != nil {
			return ResolvedSlackConnection{}, false, fmt.Errorf("scan slack connection: %w", err)
		}
		resolved = append(resolved, r)
	}
	if err := rows.Err(); err != nil {
		return ResolvedSlackConnection{}, false, fmt.Errorf("resolve slack connection: %w", err)
	}
	switch len(resolved) {
	case 0:
		return ResolvedSlackConnection{}, false, nil
	case 1:
		return resolved[0], true, nil
	}
	// Both ids are logged so an operator can see which two rows to reconcile. The team id is NOT logged: it
	// arrives on an unauthenticated request, and our own connection ids answer the question anyway.
	log.Printf("slack: workspace resolves to connections %s and %s — refusing to guess which tenant an event belongs to",
		resolved[0].ID, resolved[1].ID)
	return ResolvedSlackConnection{}, false, fmt.Errorf("%w: %s, %s", ErrSlackConnectionAmbiguous, resolved[0].ID, resolved[1].ID)
}

// CorrelateThreadSession resolves the canonical session for a (team, channel, thread), creating the mapping
// on the first event and REUSING it on every later event in the same thread (SLK-003). It returns the
// canonical session id and whether this call created it.
//
// PRECISELY WHAT THE UNIQUE INDEX BUYS, because the difference has already been over-claimed once: a
// concurrent race collapses THE ROW, so the loser reads the winner's session and only one correlation
// exists. It does NOT collapse the SESSIONS — two callers that each minted a session before calling this
// still have two sessions, and the loser's is simply not the one the thread points at. Serializing that is
// the caller's job (SlackAdmitter.lockThread holds a thread-keyed advisory lock across the whole
// first-event window); this function alone cannot provide it.
func (s *Store) CorrelateThreadSession(ctx context.Context, project, connID, team, channel, thread, sessionID string) (string, bool, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	id := newID("slkts")
	var inserted string
	err := s.pool.QueryRow(ctx, storage.Query("CorrelateThreadSession"),
		id, project, connID, team, channel, thread, sessionID).Scan(&inserted)
	switch {
	case err == nil:
		return sessionID, true, nil // fresh claim — this call's session is canonical
	case errors.Is(err, pgx.ErrNoRows):
		// The thread was already correlated (ON CONFLICT DO NOTHING returned no row): reuse the winner.
		existing, _, rerr := s.threadSession(ctx, project, team, channel, thread)
		if rerr != nil {
			return "", false, rerr
		}
		return existing, false, nil
	default:
		return "", false, fmt.Errorf("correlate thread session: %w", err)
	}
}

// RecordSlackMessageTurn records which turn a Slack message became (000042), so a later edit or deletion of
// that message can act on it. Best-effort by design and by the caller: the run is already durable when this
// writes, and a lost handle costs a retraction, never a run.
//
// requesterUserID (E21 T3, 000043) is written HERE because this is the only durable write that happens with
// the event in hand: it is what lets the reply pump address the person who asked, minutes later and across a
// restart. It never reaches a prompt.
func (s *Store) RecordSlackMessageTurn(ctx context.Context, project, connID, team, channel, messageTS, responseID, sessionID, requesterUserID string) error {
	if messageTS == "" || responseID == "" {
		return nil // an event with no message ts names no turn; there is nothing to point at
	}
	ctx = storage.ScopeToTenant(ctx, project)
	if _, err := s.pool.Exec(ctx, storage.Query("RecordSlackMessageTurn"),
		newID("slkmt"), project, connID, team, channel, messageTS, responseID, sessionID, requesterUserID); err != nil {
		return fmt.Errorf("record slack message turn: %w", err)
	}
	return nil
}

// RetractSlackMessageTurn withdraws the turn a now-deleted Slack message opened, and SupersedeSlackMessageTurn
// rewrites the turn an edited one opened. Both return the affected response id, or "" when this message never
// became a turn here — the ordinary case, since most messages in a channel birth nothing at all.
//
// One helper serves both because they differ only in the statement: same tenant scope, same handle lookup,
// same "no such turn is not an error".
func (s *Store) RetractSlackMessageTurn(ctx context.Context, project, team, channel, messageTS string) (string, error) {
	return s.reviseSlackTurn(ctx, "RetractSlackMessageTurn", project, team, channel, messageTS)
}

// SupersedeSlackMessageTurn replaces the stored turn's WORDS with `input` — the corrected text, already
// rendered by slackTurnText so the stored turn and a fresh one read identically. Only the words: a turn that
// carried an image keeps it (see the query), because an edit changes what was said, not what was shared.
func (s *Store) SupersedeSlackMessageTurn(ctx context.Context, project, team, channel, messageTS, input string) (string, error) {
	return s.reviseSlackTurn(ctx, "SupersedeSlackMessageTurn", project, team, channel, messageTS, input)
}

func (s *Store) reviseSlackTurn(ctx context.Context, query, project, team, channel, messageTS string, extra ...any) (string, error) {
	if messageTS == "" {
		return "", nil
	}
	args := append([]any{project, team, channel, messageTS}, extra...)
	var responseID string
	switch err := s.pool.QueryRow(storage.ScopeToTenant(ctx, project), storage.Query(query), args...).Scan(&responseID); {
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil // this message never became a turn in this tenant
	case err != nil:
		return "", fmt.Errorf("revise slack turn: %w", err)
	}
	return responseID, nil
}

// threadSession reads the canonical session (and its last visible bot message ts) a thread resolved to.
func (s *Store) threadSession(ctx context.Context, project, team, channel, thread string) (string, string, error) {
	var sessionID, lastTS string
	err := s.pool.QueryRow(ctx, storage.Query("GetThreadSession"), project, team, channel, thread).Scan(&sessionID, &lastTS)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrSlackThreadSessionNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("read thread session: %w", err)
	}
	return sessionID, lastTS, nil
}

// decodeSlackInput strict-decodes the create body; an unknown field (an inline secret VALUE among them) is
// rejected, so a credential can only ever arrive as a *_ref handle.
func decodeSlackInput(raw []byte) (SlackConnectionInput, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var in SlackConnectionInput
	if err := dec.Decode(&in); err != nil {
		// An inline `signing_secret`/`bot_token` VALUE (or any other unknown field) lands here — the
		// decodeMCPConnectionInput precedent maps every strict-decode failure to ErrUnknownField.
		return SlackConnectionInput{}, fmt.Errorf("%w: %v", ErrUnknownField, err)
	}
	return in, nil
}

func orEmptyList(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func orEmptyObject(v map[string]any) map[string]any {
	if v == nil {
		return map[string]any{}
	}
	return v
}
