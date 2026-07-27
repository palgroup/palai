package slack

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// The manifest is the ONE part of this integration nobody compiles: an operator pastes it into
// api.slack.com and Slack starts delivering whatever it names. E20 T2 found what that costs — subscribing to
// app_home_opened without writing code for it made every panel open birth a run with an empty prompt,
// because MapEvent classified it as KindOther and mapped cleanly.
//
// So this file is the drift guard the manifest never had: every event the manifest SUBSCRIBES to must appear
// in the table below with a deliberate verdict about what this adapter does with it. Adding an event to the
// manifest without deciding fails here rather than in a workspace.
//
// It is not a contract test. It cannot tell you Slack still sends these; only §6 leg 1 can.

// manifestPath is the pasted-into-Slack app configuration, relative to this package.
const manifestPath = "../../../deploy/slack/app-manifest.yaml"

// subscribedEventVerdicts is what MapEvent does with each subscribed bot event, and the field is `runs`:
// whether the event can reach admission at all. A `false` here must be backed by noRun() refusing the inner
// type — the two are asserted against each other below, so a verdict cannot drift away from the code.
var subscribedEventVerdicts = map[string]struct {
	innerType string
	runs      bool
}{
	"app_mention":         {"app_mention", true},          // SLK-001 — the original surface
	"message.channels":    {"message", true},              // SLK-002/005 — a channel message
	"message.im":          {"message", true},              // SLK-010 — the agent panel's conversation; a DM message IS a message
	"app_home_opened":     {"app_home_opened", false},     // SLK-010 — the panel was opened; not a turn
	"app_context_changed": {"app_context_changed", false}, // SLK-010/011 — what the human is looking at; not a turn
}

type slackManifest struct {
	Features struct {
		AgentView struct {
			AgentDescription string `yaml:"agent_description"`
			Actions          []any  `yaml:"actions"`
		} `yaml:"agent_view"`
	} `yaml:"features"`
	OAuthConfig struct {
		Scopes struct {
			Bot []string `yaml:"bot"`
		} `yaml:"scopes"`
	} `yaml:"oauth_config"`
	Settings struct {
		EventSubscriptions struct {
			BotEvents []string `yaml:"bot_events"`
		} `yaml:"event_subscriptions"`
	} `yaml:"settings"`
}

func readManifest(t *testing.T) slackManifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(manifestPath))
	if err != nil {
		t.Fatalf("read the app manifest: %v", err)
	}
	var m slackManifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatalf("the app manifest is not valid YAML — Slack will refuse the paste: %v", err)
	}
	return m
}

// TestManifestSubscribesToNothingTheAdapterHasNotDecided is the guard proper: an event nobody wrote code for
// does not fail loudly, it births a junk run.
func TestManifestSubscribesToNothingTheAdapterHasNotDecided(t *testing.T) {
	m := readManifest(t)
	if len(m.Settings.EventSubscriptions.BotEvents) == 0 {
		t.Fatal("the manifest subscribes to no bot events; the fixture, not the code, is wrong")
	}
	for _, name := range m.Settings.EventSubscriptions.BotEvents {
		verdict, known := subscribedEventVerdicts[name]
		if !known {
			t.Fatalf("the manifest subscribes to %q and this adapter has no verdict for it. Decide first: does it birth a run (add it to subscribedEventVerdicts and to MapEvent's classification) or is it a surface event (add it to noRun)? An undecided subscription births a run with an empty prompt.", name)
		}
		if got := !noRun(verdict.innerType); got != verdict.runs {
			t.Fatalf("%q: the table says runs=%t but noRun(%q) says runs=%t — the verdict drifted away from the code",
				name, verdict.runs, verdict.innerType, got)
		}
	}
	// The reverse direction: a no-run inner type the manifest does NOT subscribe to is dead code, but a
	// SUBSCRIBED one missing from the table is the dangerous direction and is covered above.
	for name, verdict := range subscribedEventVerdicts {
		if verdict.runs {
			continue
		}
		if !noRun(verdict.innerType) {
			t.Fatalf("%q is declared a surface event but noRun(%q) is false", name, verdict.innerType)
		}
	}
}

// TestManifestAgentViewHonoursThePublishedLimits pins the two facts the vendor page states and the one §5
// decision that would otherwise be invisible in a document nobody compiles.
//
// CONTRACT: https://docs.slack.dev/reference/app-manifest/ (checked 2026-07-27) — agent_description is
// required inside agent_view and its "Maximum length is 300 characters"; suggested_prompts and actions are
// optional arrays.
func TestManifestAgentViewHonoursThePublishedLimits(t *testing.T) {
	av := readManifest(t).Features.AgentView
	if av.AgentDescription == "" {
		t.Fatal("features.agent_view.agent_description is empty; it is REQUIRED when agent_view is declared")
	}
	if n := len(av.AgentDescription); n > 300 {
		t.Fatalf("agent_description is %d characters, and the published maximum is 300 — Slack refuses the manifest, and it refuses it at paste time in a browser rather than here", n)
	}
	if len(av.Actions) != 0 {
		t.Fatalf("features.agent_view.actions[] declares %d action(s). An action is an INVOCABLE surface: every actionable element in this integration is minted by interactions.go and passes the approval chain, and one declared here would have no authorization path at all (E20 plan §5).", len(av.Actions))
	}
}

// TestManifestGrantsTheScopesTheSubscribedEventsRequire: an event Slack will not deliver is worse than one we
// do not handle, because nothing anywhere reports it — the panel is simply silent.
//
// CONTRACT (all checked 2026-07-27): message.im requires `im:history`
// (https://docs.slack.dev/reference/events/message.im/); app_context_changed requires `assistant:write` AND
// agent_view enabled (https://docs.slack.dev/reference/events/app_context_changed/); app_home_opened requires
// NO scope ("No scopes required!", https://docs.slack.dev/reference/events/app_home_opened/); app_mention
// requires `app_mentions:read` and message.channels requires `channels:history` (E19).
func TestManifestGrantsTheScopesTheSubscribedEventsRequire(t *testing.T) {
	m := readManifest(t)
	required := map[string]string{
		"app_mention":         "app_mentions:read",
		"message.channels":    "channels:history",
		"message.im":          "im:history",
		"app_context_changed": "assistant:write",
		"app_home_opened":     "", // documented as needing none
	}
	granted := map[string]bool{}
	for _, s := range m.OAuthConfig.Scopes.Bot {
		granted[s] = true
	}
	for _, name := range m.Settings.EventSubscriptions.BotEvents {
		scope, known := required[name]
		if !known {
			t.Fatalf("no scope requirement is recorded for the subscribed event %q; look it up on its reference page before subscribing", name)
		}
		if scope != "" && !granted[scope] {
			t.Fatalf("the manifest subscribes to %q but does not grant %q — Slack silently never delivers the event, and the surface that depends on it is simply dead", name, scope)
		}
	}
	// The panel itself. agent_view is declared, so assistant:write must be present: Slack adds it
	// automatically when the feature is enabled, and a manifest that leaves it implicit hides which of its
	// scopes came from where.
	if m.Features.AgentView.AgentDescription != "" && !granted["assistant:write"] {
		t.Fatal("agent_view is declared but assistant:write is not written out; Slack adds it on enable, and an implicit scope is one nobody reviews")
	}
}
