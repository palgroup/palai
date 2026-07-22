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
