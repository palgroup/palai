// Package botcreds joins two inventories that deliberately know nothing about each other: the
// kind-agnostic bot registry (internal/bots, whose charter is that no method of its own decodes a bot's
// `config`) and the secret store (internal/identity, whose charter is that a sealed value has no
// read-back path). Answering "what are this bot's credentials" requires reading that config AND opening
// those secrets, so it belongs to neither package; it lives here, as one type with one method.
//
// It is also where the `*_ref` naming convention is stated exactly once. The console writes those keys
// (apps/web-console/lib/channels.ts), each channel's relay process reads them, and this resolver is the
// third party to the agreement: it turns the NAMES a row carries into VALUES, for that row only.
package botcreds

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/coordinator"
)

// refSuffix is the convention: a config key ending in it names a sealed secret rather than holding a
// value. It is the whole vocabulary this package has about a bot's document — no key name, no channel, no
// kind appears anywhere below.
const refSuffix = "_ref"

// Registry is the tenant-scoped registry read this resolver runs on; *bots.Store satisfies it. The method
// is deliberately api.BotRegistry's own GetBot rather than a new narrower one: the tenant scoping, the RLS
// path and the missing-or-foreign 404 are then not IMITATED from GET /v1/bots/{bot_id}, they are the same
// code, and a change to how that read is scoped cannot leave this route behind.
type Registry interface {
	GetBot(ctx context.Context, scope middleware.Scope, id string) (api.BotResult, error)
}

// Secrets is the redemption seam; *identity.SecretStore satisfies it. ok=false means no secret is sealed
// under that name FOR THIS TENANT — a configuration fact, not an error.
//
// It said "in this installation" and that was accurate until 000006: the store resolved under
// storage.WithInstallationScope, so a bot in project A and a bot in project B naming the same handle
// redeemed the same bytes. The tenant argument is what makes the sentence above narrower than the
// installation, and ResolveBotCredentials passes the caller's verified scope into it.
type Secrets interface {
	Resolve(ctx context.Context, tenant coordinator.Tenant, name string) ([]byte, bool, error)
}

// Resolver implements api.BotCredentials.
type Resolver struct {
	registry Registry
	secrets  Secrets
}

// Compile-time proof Resolver satisfies the router's seam.
var _ api.BotCredentials = (*Resolver)(nil)

// New builds the resolver.
func New(registry Registry, secrets Secrets) *Resolver {
	return &Resolver{registry: registry, secrets: secrets}
}

// ResolveBotCredentials reads one bot row within the caller's verified scope and opens the secrets its own
// config names.
//
// THE ORDER IS THE SECURITY PROPERTY. The registry read comes first and a miss returns immediately, so a
// caller naming a bot in another tenant — or no bot at all — reaches the secret store zero times. Only
// names taken out of the row that read returned are ever passed to Resolve, and that row is in the
// caller's own project by construction.
func (r *Resolver) ResolveBotCredentials(ctx context.Context, scope middleware.Scope, botID string) (api.BotCredentialsResult, error) {
	row, err := r.registry.GetBot(ctx, scope, botID)
	if err != nil {
		return api.BotCredentialsResult{}, err
	}
	if row.NotFound {
		return api.BotCredentialsResult{NotFound: true}, nil
	}

	handles, err := refHandles(row.Body)
	if err != nil {
		return api.BotCredentialsResult{}, err
	}
	result := api.BotCredentialsResult{Values: make(map[string][]byte, len(handles))}
	if len(handles) == 0 {
		// A row that names no handle is a real answer, not a miss: an empty map says "this bot carries no
		// sealed credentials", which is exactly true and tells the caller nothing about what exists.
		return result, nil
	}

	for _, h := range handles {
		if h.name == "" {
			result.Unresolved = append(result.Unresolved, h.key)
			continue
		}
		value, ok, err := r.secrets.Resolve(ctx, coordinator.Tenant{Project: scope.Project}, h.name)
		if err != nil {
			// Named by CONFIG KEY. The wrapped error still carries the secret's name (the store puts it
			// there) and that is fine internally — a name is not a value — but the key is what an operator
			// repairs, and the API layer renders none of this anyway.
			return api.BotCredentialsResult{}, fmt.Errorf("redeem handle named by %s: %w", h.key, err)
		}
		if !ok {
			result.Unresolved = append(result.Unresolved, h.key)
			continue
		}
		result.Values[h.key] = value
	}
	return result, nil
}

// handle is one config key and the secret name it carries.
type handle struct{ key, name string }

// refHandles reads the handles out of a bot projection: every TOP-LEVEL key of `config` whose name ends in
// `_ref`, in key order.
//
// TOP-LEVEL AND STRING-VALUED ONLY, and both halves are deliberate. The convention the console writes is
// flat, and walking a document to arbitrary depth would make the set of names this route resolves depend
// on a shape nobody bounded — a nested object could then grow a `_ref` key that no reader ever agreed to.
// A key whose value is not a string, or is the empty string, yields an empty name rather than being
// dropped: the caller is told the key is unresolved instead of finding it silently absent.
//
// It reads the rendered projection rather than a row struct because that projection is what the tenant-
// scoped read returns, and re-reading the row by another path would be a second, unproven way to the same
// bytes.
func refHandles(body []byte) ([]handle, error) {
	var projection struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(body, &projection); err != nil {
		return nil, fmt.Errorf("read bot config: %w", err)
	}
	keys := make([]string, 0, len(projection.Config))
	for key := range projection.Config {
		if strings.HasSuffix(key, refSuffix) {
			keys = append(keys, key)
		}
	}
	// Sorted so the resolution order, and therefore Unresolved, is deterministic: a map range would make
	// two identical requests answer with the same set in a different order.
	sort.Strings(keys)

	handles := make([]handle, 0, len(keys))
	for _, key := range keys {
		var name string
		if err := json.Unmarshal(projection.Config[key], &name); err != nil {
			handles = append(handles, handle{key: key})
			continue
		}
		handles = append(handles, handle{key: key, name: name})
	}
	return handles, nil
}
