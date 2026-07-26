package execution_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
)

// TestEnrollmentTokenIsRateLimitedNotOneUse pins the redemption rule the expired-identity
// recovery path depends on, and the bound that replaced one-use. The token is a bootstrap
// credential a runner may re-present when it holds no valid identity — so a SECOND redemption
// must be admitted — but not at will: two redemptions inside one issued-certificate lifetime are
// refused, so a leaked token mints at most one certificate per lifetime rather than a fleet.
func TestEnrollmentTokenIsRateLimitedNotOneUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner-token")
	if err := os.WriteFile(path, []byte("bootstrap-token\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	tokens := execution.NewFileEnrollmentTokens(path, 300*time.Millisecond)

	if err := tokens.Consume("bootstrap-token"); err != nil {
		t.Fatalf("first redemption: %v", err)
	}
	// The rate limit, not one-use: an immediate replay is refused...
	if err := tokens.Consume("bootstrap-token"); err == nil {
		t.Fatal("a second redemption inside one certificate lifetime was admitted; the token mints a fleet")
	}
	// ...but once a lifetime has elapsed the runner can recover from an expired identity.
	time.Sleep(350 * time.Millisecond)
	if err := tokens.Consume("bootstrap-token"); err != nil {
		t.Fatalf("re-enrollment after one certificate lifetime was refused: %v — an expired identity is unrecoverable", err)
	}

	// Nothing else is admitted: the token must match the CURRENT file, and an empty token is
	// never a credential.
	if err := tokens.Consume("some-other-token"); err == nil {
		t.Fatal("a token that does not match the file was admitted")
	}
	if err := tokens.Consume(""); err == nil {
		t.Fatal("an empty token was admitted")
	}
	// A rotated file (a re-up mints a fresh token) invalidates the old one immediately.
	if err := os.WriteFile(path, []byte("rotated-token\n"), 0o600); err != nil {
		t.Fatalf("rotate token file: %v", err)
	}
	if err := tokens.Consume("bootstrap-token"); err == nil {
		t.Fatal("the pre-rotation token was still admitted")
	}
}
