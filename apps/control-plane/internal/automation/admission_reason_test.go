package automation

import (
	"strings"
	"testing"

	"github.com/palgroup/palai/packages/coordinator"
)

// TestAdmissionFailureReasonPurgedFailsClosed pins the T2 residual: a triggered-delivery admission that
// replays onto a PURGED (reaped/tombstoned) response must fail the delivery closed, not fall through and
// record a delivery pointing at a ghost run. The reason names the purge so the failure is diagnosable,
// distinct from the generic admission conflict.
func TestAdmissionFailureReasonPurgedFailsClosed(t *testing.T) {
	got := admissionFailureReason(coordinator.Admission{Purged: true})
	if !strings.Contains(got, "purged") {
		t.Fatalf("admissionFailureReason(Purged) = %q, want a purged fail-closed reason", got)
	}
}
