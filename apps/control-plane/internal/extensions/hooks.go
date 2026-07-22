package extensions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/packages/egress"
	"github.com/palgroup/palai/storage"
)

// The hooks registry management surface (spec §28.17, E12 Task 8, TOL-012). A hook is an admin-registered
// extension point that fires INSIDE the run's single dispatch loop at one of five pinned points. Create +
// disable are admin actions (never model-facing — a test pins the absence of any model-callable hook-register
// tool). This file owns the management writes/reads + the create-time validation; the dispatch (Fire) that
// runs a point's hooks in registration order lives in hook_dispatch.go.

var (
	// ErrUnknownHookPoint is returned when a hook names a point outside the five pinned points — dead config
	// (a hook nothing fires) is a create reject, never a stored row.
	ErrUnknownHookPoint = errors.New("extensions: hook_point must be one of before_tool|after_tool|before_model|on_terminal|before_repository_publish")
	// ErrInvalidHookCategory is returned when a hook names a category outside policy|transform|observer.
	ErrInvalidHookCategory = errors.New("extensions: hook category must be policy|transform|observer")
	// ErrHookMatrixViolation is returned when a (category, point) pair is outside the allowed matrix (spec
	// §28.17): a transform where there is no arguments/result to patch, or a policy where the effect already
	// ran or nothing can be denied.
	ErrHookMatrixViolation = errors.New("extensions: hook category is not allowed at this point (category × point matrix)")
	// ErrInvalidHookExecutor is returned for an executor other than platform_inline|remote_http.
	ErrInvalidHookExecutor = errors.New("extensions: hook executor must be platform_inline or remote_http")
	// ErrInvalidHookConfig is returned when the executor-specific config is missing/malformed: a
	// platform_inline hook needs a handler; a remote_http hook needs a vettable https url + a secret_ref
	// handle. A credential is NEVER inline (DisallowUnknownFields rejects an unknown field first).
	ErrInvalidHookConfig = errors.New("extensions: hook config is invalid for its executor")
	// ErrInvalidHookName is returned for an empty or over-long hook name (the admin management key).
	ErrInvalidHookName = errors.New("extensions: hook name must be non-empty and within the length bound")
	// ErrHookNameCollision is returned when a hook name is already taken in the project.
	ErrHookNameCollision = errors.New("extensions: hook name already exists in this project")
	// ErrHookNotFound is returned when a disable/read targets a hook absent from scope.
	ErrHookNotFound = errors.New("extensions: hook not found in scope")
)

// The five pinned hook points (spec §28.17). A closed set — app-validated, no SQL CHECK (a new point needs
// no migration). before_tool/after_tool fire at the tool_dispatch seam; before_model at model_dispatch;
// on_terminal at finalize; before_repository_publish at the publication path.
const (
	HookPointBeforeTool              = "before_tool"
	HookPointAfterTool               = "after_tool"
	HookPointBeforeModel             = "before_model"
	HookPointOnTerminal              = "on_terminal"
	HookPointBeforeRepositoryPublish = "before_repository_publish"
)

// The three hook categories (spec §28.17). policy = sync fail-CLOSED (a deny blocks the guarded operation
// visibly); transform = a schema-validated patch to before_tool.arguments / after_tool.result, fail-CLOSED;
// observer = async fail-OPEN (a crash never affects the operation).
const (
	HookCategoryPolicy    = "policy"
	HookCategoryTransform = "transform"
	HookCategoryObserver  = "observer"
)

// The two hook executors (spec §28.17). platform_inline names a code-defined deterministic, network-less
// handler; remote_http reuses the T4 signed remote-worker transport (tenant hook code NEVER runs in-process).
const (
	HookExecutorInline = "platform_inline"
	HookExecutorRemote = "remote_http"
)

// hookMatrix is the allowed (category → points) matrix (spec §28.17). A pair outside it is a create reject.
var hookMatrix = map[string]map[string]bool{
	HookCategoryPolicy: {
		HookPointBeforeTool:              true,
		HookPointBeforeModel:             true,
		HookPointBeforeRepositoryPublish: true,
	},
	HookCategoryTransform: {
		HookPointBeforeTool: true,
		HookPointAfterTool:  true,
	},
	HookCategoryObserver: {
		HookPointBeforeTool:              true,
		HookPointAfterTool:               true,
		HookPointBeforeModel:             true,
		HookPointOnTerminal:              true,
		HookPointBeforeRepositoryPublish: true,
	},
}

// maxHookNameLen bounds a hook name (the admin management key, unique per project).
const maxHookNameLen = 128

// HookInput is the strict-decoded create body (spec §28.17). Any field outside this struct — including a raw
// credential — is rejected by DisallowUnknownFields, so a secret can only enter as a secret_ref HANDLE. The
// org/project come from the verified scope, never the body (§39.2).
type HookInput struct {
	Name      string         `json:"name"`
	HookPoint string         `json:"hook_point"`
	Category  string         `json:"category"`
	Executor  string         `json:"executor"`
	Config    map[string]any `json:"config"`
	SecretRef string         `json:"secret_ref"`
	TimeoutMS *int           `json:"timeout_ms"`
}

// Hook is a registered hook's committed shape (management + read-back). Config carries only NON-secret
// wiring; a credential is the SecretRef handle.
type Hook struct {
	ID        string
	Name      string
	HookPoint string
	Category  string
	Executor  string
	Config    map[string]any
	SecretRef string
	TimeoutMS *int
	Disabled  bool
}

// DecodeHookInput strictly decodes + validates the create body: the closed-set point/category/executor, the
// (category × point) matrix, and the executor-specific config. A raw credential field is rejected first by
// DisallowUnknownFields; a malformed remote/inline config is a typed ErrInvalidHookConfig.
func DecodeHookInput(raw []byte) (HookInput, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var in HookInput
	if err := dec.Decode(&in); err != nil {
		return HookInput{}, fmt.Errorf("%w: %v", ErrUnknownField, err)
	}
	if in.Name == "" || len(in.Name) > maxHookNameLen {
		return HookInput{}, fmt.Errorf("%w: got %q", ErrInvalidHookName, in.Name)
	}
	if !knownHookPoint(in.HookPoint) {
		return HookInput{}, fmt.Errorf("%w: got %q", ErrUnknownHookPoint, in.HookPoint)
	}
	points, ok := hookMatrix[in.Category]
	if !ok {
		return HookInput{}, fmt.Errorf("%w: got %q", ErrInvalidHookCategory, in.Category)
	}
	if !points[in.HookPoint] {
		return HookInput{}, fmt.Errorf("%w: category %q at point %q", ErrHookMatrixViolation, in.Category, in.HookPoint)
	}
	if err := validateHookExecutor(in); err != nil {
		return HookInput{}, err
	}
	return in, nil
}

// knownHookPoint reports whether p is one of the five pinned points (observer's row is the full set).
func knownHookPoint(p string) bool { return hookMatrix[HookCategoryObserver][p] }

// validateHookExecutor enforces the executor + its non-secret config. platform_inline needs a handler;
// remote_http needs a vettable https url + a secret_ref handle, and ALLOWLISTS config keys so a credential
// can never land inline (the mcp-connection pattern).
func validateHookExecutor(in HookInput) error {
	switch in.Executor {
	case HookExecutorInline:
		handler, _ := in.Config["handler"].(string)
		if handler == "" {
			return fmt.Errorf("%w: platform_inline needs a handler", ErrInvalidHookConfig)
		}
		return allowlistHookConfigKeys(in.Config, "handler")
	case HookExecutorRemote:
		url, _ := in.Config["url"].(string)
		if url == "" {
			return fmt.Errorf("%w: remote_http needs a url", ErrInvalidHookConfig)
		}
		// Static egress gate at REGISTRATION: an internal literal, an http downgrade, or a credential-embedding
		// url is rejected here (the pinned dialer inside the T4 executor stays the authoritative connect-time
		// gate). A remote hook without a secret_ref cannot sign — a signed transport needs a secret.
		if err := egress.VetURL(url, false); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidHookConfig, err)
		}
		if in.SecretRef == "" {
			return fmt.Errorf("%w: remote_http needs a secret_ref (a signed transport needs a secret)", ErrInvalidHookConfig)
		}
		return allowlistHookConfigKeys(in.Config, "url", "allow_private")
	default:
		return fmt.Errorf("%w: got %q", ErrInvalidHookExecutor, in.Executor)
	}
}

// allowlistHookConfigKeys rejects any config key outside the executor's non-secret allowlist — the guard
// that keeps a credential out of the hook row entirely (it can only enter as a secret_ref handle).
func allowlistHookConfigKeys(config map[string]any, allowed ...string) error {
	set := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		set[k] = true
	}
	for k := range config {
		if !set[k] {
			return fmt.Errorf("%w: unexpected config key %q (a credential must be a secret_ref, never inline)", ErrInvalidHookConfig, k)
		}
	}
	return nil
}

// CreateHook registers a hook after strict validation. It is an admin action — never reachable from a tool
// the model can call. A duplicate name in the project is a typed collision reject.
func (s *Store) CreateHook(ctx context.Context, org, project string, raw []byte) (Hook, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	in, err := DecodeHookInput(raw)
	if err != nil {
		return Hook{}, err
	}
	if in.TimeoutMS != nil && *in.TimeoutMS > MaxTimeoutMS {
		return Hook{}, fmt.Errorf("%w: got %d ms (ceiling %d)", ErrTimeoutTooLarge, *in.TimeoutMS, MaxTimeoutMS)
	}
	id := newID("hook")
	if _, err := s.pool.Exec(ctx, storage.Query("InsertHook"),
		id, org, project, in.Name, in.HookPoint, in.Category, in.Executor,
		marshalJSON(in.Config), nullableText(in.SecretRef), in.TimeoutMS); err != nil {
		if isUniqueViolation(err) {
			return Hook{}, ErrHookNameCollision
		}
		return Hook{}, fmt.Errorf("insert hook: %w", err)
	}
	return Hook{ID: id, Name: in.Name, HookPoint: in.HookPoint, Category: in.Category, Executor: in.Executor, Config: in.Config, SecretRef: in.SecretRef, TimeoutMS: in.TimeoutMS}, nil
}

// GetHook reads a hook's committed shape (admin read-back + the CRUD roundtrip), disabled or not.
func (s *Store) GetHook(ctx context.Context, org, project, id string) (Hook, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	h := Hook{ID: id}
	var configJSON []byte
	var secretRef *string
	err := s.pool.QueryRow(ctx, storage.Query("GetHook"), id, org, project).
		Scan(&h.ID, &h.Name, &h.HookPoint, &h.Category, &h.Executor, &configJSON, &secretRef, &h.TimeoutMS, &h.Disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return Hook{}, ErrHookNotFound
	}
	if err != nil {
		return Hook{}, fmt.Errorf("read hook: %w", err)
	}
	h.Config = decodeSchema(configJSON)
	if secretRef != nil {
		h.SecretRef = *secretRef
	}
	return h, nil
}

// loadedHook is a hook resolved from the registry, ready to fire: the non-secret wiring the dispatcher
// needs plus the secret_ref handle (resolved fresh per invoke, never held). Config is flattened to its
// executor-specific fields so the dispatch core is DB-free and unit-testable.
type loadedHook struct {
	ID           string
	Point        string
	Category     string
	Executor     string
	Handler      string // platform_inline: the code-defined handler name
	URL          string // remote_http: the invoke url
	AllowPrivate bool   // remote_http: whether egress may reach a private address
	SecretRef    string // remote_http: the signing-credential handle (empty ⇒ unsigned ⇒ fail-closed)
	TimeoutMS    *int   // clamped to the category ceiling at fire time
}

// loadHooks reads a project's ENABLED hooks for one point in deterministic (created_at, id) registration
// order — the ONLY read the run dispatch loop issues per fire point. A tenant with no hooks at the point
// returns an empty slice (Fire is then a no-op).
func (s *Store) loadHooks(ctx context.Context, org, project, point string) ([]loadedHook, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	rows, err := s.pool.Query(ctx, storage.Query("HooksForPoint"), org, project, point)
	if err != nil {
		return nil, fmt.Errorf("load hooks for %q: %w", point, err)
	}
	defer rows.Close()
	var out []loadedHook
	for rows.Next() {
		var (
			h          loadedHook
			configJSON []byte
			secretRef  *string
		)
		if err := rows.Scan(&h.ID, &h.Point, &h.Category, &h.Executor, &configJSON, &secretRef, &h.TimeoutMS); err != nil {
			return nil, fmt.Errorf("scan hook: %w", err)
		}
		cfg := decodeSchema(configJSON)
		h.Handler, _ = cfg["handler"].(string)
		h.URL, _ = cfg["url"].(string)
		h.AllowPrivate, _ = cfg["allow_private"].(bool)
		if secretRef != nil {
			h.SecretRef = *secretRef
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate hooks: %w", err)
	}
	return out, nil
}

// DisableHook flips the admin kill-switch once. Reports whether the hook existed in scope.
func (s *Store) DisableHook(ctx context.Context, org, project, id string) (bool, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	switch err := s.pool.QueryRow(ctx, storage.Query("DisableHook"), id, org, project).Scan(new(string)); {
	case err == nil:
		return true, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return false, fmt.Errorf("disable hook: %w", err)
	}
	// No flip: either already-disabled or unknown. Existence disambiguates.
	switch err := s.pool.QueryRow(ctx, storage.Query("HookExists"), id, org, project).Scan(new(int)); {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("check hook: %w", err)
	}
	return true, nil
}
