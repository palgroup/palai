//go:build live

// This is the E16 T6 live cancel leg. It runs only under the `live` build tag, in
// `make test-live-provider PROVIDER=provider-one CASE=cancel-terminal`, which loads the real
// credential from .env.local at runtime. It proves a REAL streamed provider-one completion is
// honored when the caller cancels mid-stream — the call terminates with context.Canceled (never a
// fabricated completed result) and the partial tokens the caller already saw are retained — and
// that the credential never appears in any captured surface.
package provider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// TestLiveProviderOneMidStreamCancel is CASE=cancel-terminal (MOD-009): a REAL streamed provider-one
// completion is canceled the moment the first token arrives; the call must terminate with
// context.Canceled, and the partial tokens streamed before the cancel are retained. The credential
// is used only as an opaque needle for the leak scan and is never printed.
func TestLiveProviderOneMidStreamCancel(t *testing.T) {
	secret := os.Getenv(credentialEnv)
	if secret == "" {
		t.Fatalf("%s is unset; the live tier loads it from .env.local at runtime", credentialEnv)
	}

	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})

	req := modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID("mreq_live_cancel_1"),
		RouteRevision:  1,
		ModelStepID:    "step-1",
		Model:          liveModel(),
		Messages: []modelbroker.Message{
			{Role: "user", Content: "Count from 1 to 40, one number per line."},
		},
		Deadline:    time.Now().Add(60 * time.Second),
		Reservation: modelbroker.Reservation{MaxTotalTokens: 4000},
		Secret:      modelbroker.SecretRef("provider-one"),
	}

	ctx, cancel := context.WithCancel(context.Background())
	var streamed bytes.Buffer
	var tokens int
	res, err := broker.Route(ctx, "provider-one", req, func(d modelbroker.Delta) {
		if d.Text != "" {
			streamed.WriteString(d.Text)
			tokens++
			cancel() // cancel the moment the first real token arrives — mid-stream
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-stream cancel: got err=%v, want context.Canceled (a real cancel must be honored, not completed)", err)
	}
	if tokens == 0 {
		t.Error("no partial token was delivered before cancel — the caller must retain what it saw")
	}

	// Leak scan by construction: the credential value must not appear in any captured surface.
	resultJSON, mErr := json.Marshal(res)
	if mErr != nil {
		t.Fatalf("marshal result: %v", mErr)
	}
	surfaces := map[string][]byte{"streamed tokens": streamed.Bytes(), "canonical result": resultJSON}
	for name, captured := range surfaces {
		if bytes.Contains(captured, []byte(secret)) {
			t.Fatalf("%s contains the credential value", name) // never echo the value
		}
	}

	// Safe evidence only: the partial token count and the honored-cancel verdict.
	t.Logf("live PASS cancel-terminal: partial_tokens=%d canceled=%v", tokens, errors.Is(err, context.Canceled))
}
