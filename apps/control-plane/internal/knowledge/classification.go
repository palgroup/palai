package knowledge

import "fmt"

// KNO-007: a restricted-classification source's content must NEVER be sent to a disallowed embedding
// provider or region. Classification is pinned on the source (knowledge_sources.classification, T4). The
// embedding step (the optional §25.15.2 step 7) consults this policy BEFORE a restricted source's bytes
// leave for a route, so a restricted document is never embedded outside its allowed region/provider.

// classificationRestricted is the pinned label that gates a source away from disallowed embedding targets.
const classificationRestricted = "restricted"

// EmbeddingRoute is the resolved embedding target: the pinned route ref plus the provider/region its bytes
// would travel to. The value fields are what KNO-007 checks; the ref resolves through the E13 secret_refs
// chain (never a secret value here).
type EmbeddingRoute struct {
	Ref      string
	Provider string
	Region   string
}

// EmbeddingPolicy pins the provider/region a RESTRICTED-classification source may be embedded to (KNO-007).
// Non-restricted content is unconstrained. An empty AllowedRegions means restricted content may not be
// embedded ANYWHERE (fail-closed) — a restricted source stays FTS-only until an operator pins an allowed
// region. AllowedProviders, when set, further narrows the provider.
type EmbeddingPolicy struct {
	AllowedRegions   []string
	AllowedProviders []string
}

// allows reports whether a source of the given classification may be embedded via route. A restricted source
// bound for a region/provider outside the allowlist is refused with a KNO-007 error naming the target, so
// the refusal is auditable and never silent.
func (p EmbeddingPolicy) allows(classification string, route EmbeddingRoute) error {
	if classification != classificationRestricted {
		return nil
	}
	if !contains(p.AllowedRegions, route.Region) {
		return fmt.Errorf("knowledge: restricted content may not be embedded to region %q (KNO-007)", route.Region)
	}
	if len(p.AllowedProviders) > 0 && !contains(p.AllowedProviders, route.Provider) {
		return fmt.Errorf("knowledge: restricted content may not be embedded via provider %q (KNO-007)", route.Provider)
	}
	return nil
}

func contains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
