package execution

import (
	"context"
	"encoding/json"

	remotehttp "github.com/palgroup/palai/adapters/tools/http"
	"github.com/palgroup/palai/packages/coordinator"
)

// remoteOperationReader is the narrow read the RemoteToolProber needs — the resolved result for an
// uncertain tool_call's remote operation. *remotehttp.Operations satisfies it.
type remoteOperationReader interface {
	ProberRead(ctx context.Context, toolCallID string) (state string, result []byte, found bool, err error)
}

// RemoteToolProber is the FIRST real ToolDestinationProber (spec §26.7, E12 T4): the "destination read"
// for an uncertain remote_http tool_call is the durable remote-operation ledger — a LATE signed callback
// (after the executor's deadline) wrote late_result there without ever touching the tool ledger, so the
// prober reads it and reports the effect landed. A tool_call with no resolved operation (no callback ever
// arrived) is unsupported, so the reconcile loop escalates it to manual_resolution rather than guessing.
// A non-remote uncertain call (a built-in) also has no operation row, so it too escalates to manual —
// exactly the pre-T4 behaviour, unchanged.
type RemoteToolProber struct {
	ops remoteOperationReader
}

// NewRemoteToolProber binds the remote-operation ledger the prober reads.
func NewRemoteToolProber(ops remoteOperationReader) *RemoteToolProber {
	return &RemoteToolProber{ops: ops}
}

// Probe reads the uncertain call's remote operation. A completed/late_result row carries the result the
// callback delivered, so the effect landed (applied=true -> reconciled_completed). No resolved row means
// the callback never arrived, so the prober is unsupported for this call (-> manual_resolution).
func (p *RemoteToolProber) Probe(ctx context.Context, call coordinator.UncertainToolCall) (applied bool, result []byte, supported bool, err error) {
	state, res, found, err := p.ops.ProberRead(ctx, call.CallID)
	if err != nil {
		return false, nil, false, err
	}
	if !found {
		return false, nil, false, nil // no callback ever resolved this operation
	}
	switch state {
	case "completed", "late_result":
		// Unwrap the discriminated {result|problem} envelope so the reconcile hands the model the BARE
		// payload the callback delivered (MF2), not the storage wrapper. A problem-carrying late callback
		// reconciles with its problem body — the tool answered, late, with a failure (a thin honest edge).
		payload, _, derr := remotehttp.DecodeStoredResult(res)
		if derr != nil {
			return false, nil, false, nil // an unusable stored blob → manual_resolution rather than a guess
		}
		bare, _ := json.Marshal(payload)
		return true, bare, true, nil
	default:
		return false, nil, false, nil
	}
}
