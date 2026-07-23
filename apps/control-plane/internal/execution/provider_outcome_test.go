package execution

// providerCallOutcome decides what palai_provider_errors_total counts (E14 T6, review SHOULD 3): only
// an UPSTREAM failure is an error; config problems, the platform budget cutoff, and an interrupt are
// not. This table guards that classification so the provider-error alert stays quiet when the upstream
// is healthy.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

func TestProviderCallOutcomeCountsOnlyUpstreamFailures(t *testing.T) {
	upstream := errors.New("dial provider: connection reset")
	cases := []struct {
		name      string
		routeErr  error
		result    modelbroker.Result
		wantError bool
	}{
		{"clean success", nil, modelbroker.Result{}, false},
		{"provider-side rejection on result", nil, modelbroker.Result{Error: &modelbroker.SanitizedError{Code: "rate_limited"}}, true},
		{"transport/adapter error", upstream, modelbroker.Result{}, true},
		{"interrupt cancellation", context.Canceled, modelbroker.Result{}, false},
		{"wrapped interrupt", fmt.Errorf("route: %w", context.Canceled), modelbroker.Result{}, false},
		{"unknown provider (config)", fmt.Errorf("%w: x", modelbroker.ErrUnknownProvider), modelbroker.Result{}, false},
		{"unknown secret (config)", fmt.Errorf("redeem credential: %w", modelbroker.ErrUnknownSecret), modelbroker.Result{}, false},
		{"budget cutoff", fmt.Errorf("admit: %w", modelbroker.ErrBudgetExceeded), modelbroker.Result{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := providerCallOutcome(tc.routeErr, tc.result) != nil; got != tc.wantError {
				t.Errorf("providerCallOutcome error = %v, want %v", got, tc.wantError)
			}
		})
	}
}
