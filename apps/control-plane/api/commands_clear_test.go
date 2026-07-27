package api

import (
	"testing"

	"github.com/palgroup/palai/packages/contracts"
)

// TestClearIsAcceptedAtTheHTTPSurface. commandKinds is not bookkeeping — it is the surface. The
// coordinator can apply a clear all it likes; if this map omits the kind, validateCommand answers
// 400 and no HTTP caller can ever send one, so the durable half is unreachable code.
//
// The negative case is the reason the map exists at all: an unknown kind must still be refused
// before any durable write, so a typo does not become a command row.
func TestClearIsAcceptedAtTheHTTPSurface(t *testing.T) {
	if err := validateCommand(contracts.CommandCreateRequest{CommandID: "cmd_1", Kind: "clear"}); err != nil {
		t.Fatalf("validateCommand(clear) = %v, want accepted — the kind is unreachable over HTTP without it", err)
	}
	if err := validateCommand(contracts.CommandCreateRequest{CommandID: "cmd_1", Kind: "cleer"}); err == nil {
		t.Fatal("validateCommand accepted an unknown kind; the allow-list must refuse before any durable write")
	}
	// A clear carries no content, so it must not be made to look like a send_message: no delivery
	// mode, no message body, and validation must not demand either.
	if err := validateCommand(contracts.CommandCreateRequest{CommandID: "cmd_1", Kind: "clear", Delivery: ""}); err != nil {
		t.Fatalf("validateCommand(clear with no delivery) = %v, want accepted", err)
	}
}
