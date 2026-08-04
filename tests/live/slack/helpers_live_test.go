//go:build live

// The credential-gated helpers the surviving Slack live legs share.
//
// THEY LIVE IN THEIR OWN FILE BECAUSE THE FILE THAT USED TO HOLD THEM IS GONE. Until the 2026-08-05 cutover
// they sat in events_live_test.go beside the two HTTP Events API legs; that file went with the in-process
// Slack bridge, and a helper that survives only as long as one of its callers is a helper that takes the
// other callers down with it.
//
// WHAT THE CUTOVER LEFT HERE, and the rule that decided it: a leg stays when the code it measures still
// ships. The adapter (adapters/integrations/slack) is what apps/slack-bot dials, so the Socket Mode protocol
// leg and the four chat.*Stream legs still measure production. The two Events API legs and the interactivity
// click leg measured an HTTP receiver this deployment no longer mounts, and a live test for an absent
// transport is a receipt nobody can earn.
package live

import (
	"os"
	"testing"
	"time"
)

// need returns an env var's value or skips, naming where §0 says to get it.
func need(t *testing.T, name, where string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s is not set — supply it from E19 plan §0 (%s) and re-run; no code changes are needed", name, where)
	}
	return v
}

// liveWindow is how long a test waits for the operator to act in Slack.
func liveWindow(t *testing.T) time.Duration {
	t.Helper()
	if raw := os.Getenv("PALAI_SLACK_LIVE_TIMEOUT"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			t.Fatalf("PALAI_SLACK_LIVE_TIMEOUT=%q is not a duration", raw)
		}
		return d
	}
	return 3 * time.Minute
}
