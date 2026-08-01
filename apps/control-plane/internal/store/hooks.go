package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
)

// The E12 Task 8 hooks management surface (spec §28.17, TOL-012). These adapt the tenant-scoped api.HookAPI
// contract to the extensions store: scope → (organization, project), the typed rejects → api.HookResult flags,
// and a committed row / disable summary → its JSON projection.

// CreateHook registers a hook. An unknown point/category/executor, an out-of-matrix pair, an invalid config,
// or an inline secret is a BadField (400); a name collision is a Conflict (409).
func (s *Store) CreateHook(ctx context.Context, scope middleware.Scope, body []byte) (api.HookResult, error) {
	hook, err := s.tools.CreateHook(ctx, scope.Organization, scope.Project, body)
	if res, mapped := hookReject(err); mapped {
		return res, nil
	}
	if err != nil {
		return api.HookResult{}, err
	}
	out, _ := json.Marshal(map[string]any{
		"id": hook.ID, "object": "hook", "name": hook.Name,
		"hook_point": hook.HookPoint, "category": hook.Category, "executor": hook.Executor,
	})
	return api.HookResult{Body: out}, nil
}

// GetHook reads one hook's management projection (GET /v1/hooks/{id}, E29 T1). An absent or foreign hook is
// a NotFound (404) — a tenant-scoped miss, never an existence disclosure.
func (s *Store) GetHook(ctx context.Context, scope middleware.Scope, id string) (api.HookResult, error) {
	hook, err := s.tools.GetHook(ctx, scope.Organization, scope.Project, id)
	if errors.Is(err, extensions.ErrHookNotFound) {
		return api.HookResult{NotFound: true}, nil
	}
	if err != nil {
		return api.HookResult{}, err
	}
	return api.HookResult{Body: hookProjection(hook)}, nil
}

// ListHooks pages a project's hooks (GET /v1/hooks, E29 T1), disabled ones included. Each row carries the
// SAME projection GetHook returns.
func (s *Store) ListHooks(ctx context.Context, scope middleware.Scope, q api.ListQuery) ([]api.ListRow, error) {
	window := extensions.HookWindow{CreatedGTE: q.CreatedGTE, CreatedLTE: q.CreatedLTE, Limit: q.Limit}
	if q.After != nil {
		window.AfterCreatedAt = &q.After.CreatedAt
		window.AfterID = q.After.ID
	}
	hooks, err := s.tools.ListHooks(ctx, scope.Organization, scope.Project, window)
	if err != nil {
		return nil, err
	}
	rows := make([]api.ListRow, 0, len(hooks))
	for _, hook := range hooks {
		rows = append(rows, api.ListRow{ID: hook.ID, CreatedAt: hook.CreatedAt, Body: hookProjection(hook)})
	}
	return rows, nil
}

// hookProjection is THE hook management projection — the one shape GET /v1/hooks/{id} and every row of
// GET /v1/hooks return. It is one function because two functions is how a list row and a singular read
// drift apart, and a screen that has to code against two shapes reads one of them wrong.
//
// secret_ref is a HANDLE and it is returned as one: it names where a signing credential lives, it is not
// the credential, and nothing here can resolve it. config is safe to return for a structural reason rather
// than a hopeful one — allowlistHookConfigKeys refuses any key outside the executor's non-secret set at
// CREATE, so a credential cannot be in this column to leak.
func hookProjection(hook extensions.Hook) []byte {
	out := map[string]any{
		"id": hook.ID, "object": "hook", "name": hook.Name,
		"hook_point": hook.HookPoint, "category": hook.Category, "executor": hook.Executor,
		"config": hook.Config, "secret_ref": hook.SecretRef, "timeout_ms": hook.TimeoutMS,
		"disabled": hook.Disabled, "created_at": hook.CreatedAt,
	}
	if hook.DisabledAt != nil {
		out["disabled_at"] = *hook.DisabledAt
	}
	body, _ := json.Marshal(out)
	return body
}

// DisableHook flips a hook's admin kill-switch. An unknown hook is a NotFound (404).
func (s *Store) DisableHook(ctx context.Context, scope middleware.Scope, id string) (api.HookResult, error) {
	existed, err := s.tools.DisableHook(ctx, scope.Organization, scope.Project, id)
	if err != nil {
		return api.HookResult{}, err
	}
	if !existed {
		return api.HookResult{NotFound: true}, nil
	}
	out, _ := json.Marshal(map[string]any{"id": id, "object": "hook", "disabled": true})
	return api.HookResult{Body: out}, nil
}

// hookReject maps a typed domain error to its api.HookResult reject flag.
func hookReject(err error) (api.HookResult, bool) {
	switch {
	case err == nil:
		return api.HookResult{}, false
	case errors.Is(err, extensions.ErrUnknownField),
		errors.Is(err, extensions.ErrUnknownHookPoint),
		errors.Is(err, extensions.ErrInvalidHookCategory),
		errors.Is(err, extensions.ErrHookMatrixViolation),
		errors.Is(err, extensions.ErrInvalidHookExecutor),
		errors.Is(err, extensions.ErrInvalidHookConfig),
		errors.Is(err, extensions.ErrInvalidHookName),
		errors.Is(err, extensions.ErrTimeoutTooLarge):
		return api.HookResult{BadField: true}, true
	case errors.Is(err, extensions.ErrHookNameCollision):
		return api.HookResult{Conflict: true}, true
	case errors.Is(err, extensions.ErrHookNotFound):
		return api.HookResult{NotFound: true}, true
	default:
		return api.HookResult{}, false
	}
}
