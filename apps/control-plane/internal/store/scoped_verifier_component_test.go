//go:build component

package store_test

import (
	"context"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// scopedVerifier resolves any bearer to one seeded tenant. The credential DB path is proven separately
// (identity component suite); what a test needs from this is a verified scope the shipped router will
// trust, so an effect can be asserted to land in THAT tenant and not in one a request body names.
//
// IT LIVES IN A FILE OF ITS OWN SINCE 2026-08-05. It used to sit in slack_registration_component_test.go,
// which went with the in-process Slack bridge — and it was never Slack's: session_list_journey_component_test.go
// is its other caller, and a shared stub that survives only as long as one arbitrary caller takes the
// others down with it.
type scopedVerifier struct{ scope middleware.Scope }

func (v scopedVerifier) VerifyAPIKey(context.Context, string) (middleware.Scope, error) {
	return v.scope, nil
}
