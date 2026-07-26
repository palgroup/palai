package stack

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// up_slack_test.go covers the Slack LAST MILE — the three gaps between "a workspace is registered"
// and "an @mention produces a run". Each one was live on main:
//
//  1. the registration named *_ref HANDLES that redeemed NOWHERE, because no VALUE was ever stored;
//  2. the Socket Mode connect loop was never switched on in a compose deployment; and
//  3. `palai up` reported `slack live — workspace … registered` regardless, which is more than a
//     registration with an unresolvable handle and no socket has earned.
//
// Every assert below is Docker-free: the decisions are pure functions and the observation is a
// parse of the control-plane's own log.

// TestEverySlackHandleRegisteredHasAValueStored is gap 1, and it is the whole reason
// slackSecretSlots is ONE table. A handle registered on the connection whose value is stored under a
// different name resolves to nothing, and nothing anywhere reports it: the socket simply never
// dials and the signature check simply never passes. Deriving both from the same row is what makes
// that drift impossible rather than merely unlikely.
func TestEverySlackHandleRegisteredHasAValueStored(t *testing.T) {
	get := env(
		"SLACK_TEAM_ID", "T0001",
		"SLACK_AGENT_REVISION_ID", "agr_1", "SLACK_PRINCIPAL_ID", "prn_1",
		"SLACK_SIGNING_SECRET", "8f14e45fceea167a5a36dedd4bea2543",
		"SLACK_BOT_TOKEN", "xoxb-notarealtoken",
		"SLACK_APP_TOKEN", "xapp-1-notarealtoken",
	)
	body, skip := slackRegistration(get)
	if skip != "" {
		t.Fatalf("a complete Slack environment was skipped: %s", skip)
	}
	stored := slackSecretValues(get)
	if len(stored) != 3 {
		t.Fatalf("stored %d secret refs for a fully-configured workspace, want 3", len(stored))
	}
	for _, slot := range slackSecretSlots {
		handle, ok := body[slot.field].(string)
		if !ok || handle == "" {
			t.Fatalf("the registration body carries no %s: the connection cannot use that credential at all", slot.field)
		}
		if _, ok := stored[handle]; !ok {
			t.Fatalf("%s = %q is registered but no value is stored under that name — the handle resolves nowhere (stored: %v)", slot.field, handle, keysOf(stored))
		}
	}
}

// TestSlackRegistersNoHandleItCannotRedeem is the same invariant from the other side. An operator
// with a signing secret but no app-level token must get a connection that claims only what it can
// do: a dangling app_token_ref would make the socket loop log a dial failure forever, and would put
// `app_token_ref` in the row as if Socket Mode were configured.
func TestSlackRegistersNoHandleItCannotRedeem(t *testing.T) {
	get := env(
		"SLACK_TEAM_ID", "T0001",
		"SLACK_AGENT_REVISION_ID", "agr_1", "SLACK_PRINCIPAL_ID", "prn_1",
		"SLACK_SIGNING_SECRET", "8f14e45fceea167a5a36dedd4bea2543",
	)
	body, skip := slackRegistration(get)
	if skip != "" {
		t.Fatalf("a signing-secret-only environment was skipped: %s", skip)
	}
	if _, ok := body["bot_token_ref"]; ok {
		t.Fatal("bot_token_ref was registered with no SLACK_BOT_TOKEN to store under it")
	}
	if _, ok := body["app_token_ref"]; ok {
		t.Fatal("app_token_ref was registered with no SLACK_APP_TOKEN to store under it")
	}
	stored := slackSecretValues(get)
	if len(stored) != 1 || stored["slack-signing-T0001"] == "" {
		t.Fatalf("stored = %v, want exactly the signing secret", keysOf(stored))
	}
}

// TestSocketModeIsSwitchedOnOnlyWithAnAppToken is gap 2. PALAI_SLACK_SOCKET_TEAM_ID is what
// main.startSlackSocket reads, and compose now passes it — but switching it on without an
// app-level token buys nothing: Socket Mode's ONLY authentication is that token, so the loop would
// start, fail to dial and say so on a timer. The variable follows the token, not the team id.
func TestSocketModeIsSwitchedOnOnlyWithAnAppToken(t *testing.T) {
	base := []string{"SLACK_TEAM_ID", "T0001", "SLACK_AGENT_REVISION_ID", "agr_1", "SLACK_PRINCIPAL_ID", "prn_1",
		"SLACK_SIGNING_SECRET", "8f14e45fceea167a5a36dedd4bea2543"}
	if team := slackSocketTeam(env(base...)); team != "" {
		t.Fatalf("Socket Mode was switched on for team %q with no app-level token to dial with", team)
	}
	if team := slackSocketTeam(env(append(base, "SLACK_APP_TOKEN", "xapp-1-notarealtoken")...)); team != "T0001" {
		t.Fatalf("slackSocketTeam = %q with a full Socket Mode environment, want T0001", team)
	}
	if team := slackSocketTeam(env()); team != "" {
		t.Fatalf("slackSocketTeam = %q on a stack with no Slack at all, want empty (dormant)", team)
	}
}

// TestSlackSecretsNeverAppearInOperatorProse: the values are read into memory here and nowhere else.
// The registration body and the operator-facing report must carry handles and prose only.
func TestSlackSecretsNeverAppearInOperatorProse(t *testing.T) {
	const (
		signing = "8f14e45fceea167a5a36dedd4bea2543"
		bot     = "xoxb-11111-22222-notarealbottoken"
		app     = "xapp-1-A0001-notarealapptoken"
	)
	get := env(
		"SLACK_TEAM_ID", "T0001", "SLACK_AGENT_REVISION_ID", "agr_1", "SLACK_PRINCIPAL_ID", "prn_1",
		"SLACK_SIGNING_SECRET", signing, "SLACK_BOT_TOKEN", bot, "SLACK_APP_TOKEN", app)
	body, _ := slackRegistration(get)
	for k, v := range body {
		s, ok := v.(string)
		if !ok {
			continue
		}
		for _, secret := range []string{signing, bot, app} {
			if strings.Contains(s, secret) {
				t.Fatalf("registration field %q carries a credential VALUE", k)
			}
		}
	}
	fact, line := slackReport(slackOutcome{team: "T0001", connectionID: "slkc_1", stored: 3, connected: true,
		detail: "slack socket: connected (connection slkc_1; Slack reports 1 of 10 connections open)"})
	for _, secret := range []string{signing, bot, app} {
		if strings.Contains(fact+line, secret) {
			t.Fatal("the operator-facing Slack report leaked a credential")
		}
	}
}

// TestSlackIsLiveOnlyWhenSlackSaidHello is gap 3, and it is the one that matters most. `slack live
// — workspace T0… registered` was printed for a connection whose handles resolved nowhere and whose
// socket did not exist: the capability table called it LIVE because a POST returned 201.
//
// The rule now: `live` requires an observation from the control-plane's own log that Slack sent the
// `hello` frame — the first message on an authenticated Socket Mode connection. Everything short of
// that is dormant and says what is missing.
func TestSlackIsLiveOnlyWhenSlackSaidHello(t *testing.T) {
	registered := slackOutcome{team: "T0001", connectionID: "slkc_1", stored: 3}
	fact, line := slackReport(registered)
	if fact != "" {
		t.Fatalf("a registered-but-unconnected workspace reported the fact %q, which the capability table renders as LIVE", fact)
	}
	if !strings.Contains(line, "NOT CONNECTED") {
		t.Fatalf("the step line must say the socket is not connected, got: %q", line)
	}

	connected := registered
	connected.connected = true
	connected.detail = "slack socket: connected (connection slkc_1; Slack reports 1 of 10 connections open)"
	fact, line = slackReport(connected)
	if fact == "" {
		t.Fatal("a workspace whose socket Slack answered with hello must report a live fact")
	}
	if !strings.Contains(fact, "T0001") || !strings.Contains(strings.ToLower(fact), "connected") {
		t.Fatalf("the live fact must name the workspace and the observation, got: %q", fact)
	}
	if !strings.Contains(line, "CONNECTED") {
		t.Fatalf("the step line must report the connection, got: %q", line)
	}
}

// TestSocketLogTellsTheStatesApart: the observation is a parse of the control-plane's log, so it has
// to distinguish the states an operator would act on differently. A bare "not connected" for all of
// them would send someone hunting for a Slack app problem when the real fault is that the container
// never got the variable — or leave them waiting while Slack refuses their token once a second.
//
// Every `logs` fixture below except the first is VERBATIM `docker compose logs control-plane` output
// captured from a real compose stack brought up on this branch (an isolated probe project, a real
// registration, a deliberately bogus app-level token). A hand-written fixture can only confirm what
// the person writing it already believed the log looked like: the invalid_auth case exists BECAUSE
// that probe showed the actionable line rides the supervisor, not the loop, which the first version
// of this parser threw away.
func TestSocketLogTellsTheStatesApart(t *testing.T) {
	for _, tc := range []struct {
		name      string
		logs      string
		connected bool
		want      string
	}{
		{
			name:      "hello arrived",
			logs:      "control-plane-1  | 2026/07/27 10:00:02 slack socket: connected (connection slkc_1; Slack reports 1 of 10 connections open)",
			connected: true,
			want:      "connected",
		},
		{
			name: "the loop started but the workspace is not registered yet",
			logs: "control-plane-1  | 2026/07/26 22:46:40 palai control-plane: Slack Socket Mode enabled for the configured workspace\n" +
				"control-plane-1  | 2026/07/26 22:46:40 slack socket: no enabled Slack connection is registered for the configured workspace; Socket Mode stays off until one is registered and the control plane restarts",
			want: "no enabled Slack connection is registered",
		},
		{
			name: "Slack refuses the app-level token",
			logs: "control-plane-1  | 2026/07/26 22:47:24 palai control-plane: Slack Socket Mode enabled for the configured workspace\n" +
				"control-plane-1  | 2026/07/26 22:47:25 supervised \"slack-socket\" failed (restart 1); restarting after backoff: apps.connections.open refused: \"invalid_auth\"\n" +
				"control-plane-1  | 2026/07/26 22:47:26 supervised \"slack-socket\" failed (restart 2); restarting after backoff: apps.connections.open refused: \"invalid_auth\"",
			want: "invalid_auth",
		},
		{
			name: "the loop never started at all",
			logs: "control-plane-1  | palai control-plane listening on :8080\ncontrol-plane-1  | dispatch worker 0 started",
			want: "PALAI_SLACK_SOCKET_TEAM_ID",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			connected, detail := readSlackSocketLog(tc.logs)
			if connected != tc.connected {
				t.Fatalf("connected = %t, want %t (detail: %s)", connected, tc.connected, detail)
			}
			if !strings.Contains(detail, tc.want) {
				t.Fatalf("detail = %q, want it to mention %q", detail, tc.want)
			}
		})
	}
}

// TestSocketLogReadsOnlyTheLatestState: a boot that connected, was warned, reconnected and connected
// again must read as CONNECTED. The scan takes the last thing the loop said, not the first.
func TestSocketLogReadsOnlyTheLatestState(t *testing.T) {
	logs := "cp | slack socket: no enabled Slack connection is registered for the configured workspace\n" +
		"cp | slack socket: connected (connection slkc_1; Slack reports 1 of 10 connections open)\n"
	if connected, detail := readSlackSocketLog(logs); !connected {
		t.Fatalf("a log whose LAST socket line is the hello read as not connected: %q", detail)
	}
	logs += "cp | slack socket: a connection ended (EOF); 0 still open\n"
	if connected, detail := readSlackSocketLog(logs); connected {
		t.Fatalf("a socket that has since ended read as connected: %q", detail)
	}
}

// TestMasterKeyIsMintedOnceAndNeverReplaced: the master key seals every secret in the store, so a
// second `palai up` that re-minted it would make every previously stored Slack credential
// undecryptable — and the fail-closed resolver would then refuse rather than fall back, taking the
// whole stack's Slack wiring down with it.
func TestMasterKeyIsMintedOnceAndNeverReplaced(t *testing.T) {
	p := tempPaths(t)
	if err := ensureMasterKey(p); err != nil {
		t.Fatalf("mint the master key: %v", err)
	}
	first, err := os.ReadFile(p.masterKey)
	if err != nil {
		t.Fatalf("read the minted key: %v", err)
	}
	if _, err := hex.DecodeString(strings.TrimSpace(string(first))); err != nil || len(strings.TrimSpace(string(first))) != 64 {
		t.Fatalf("the minted key is not 64 hex chars (identity.ParseMasterKey's AES-256 contract): %d chars", len(first))
	}
	info, err := os.Stat(p.masterKey)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the master key file is mode %v, want 0600", info.Mode().Perm())
	}
	if err := ensureMasterKey(p); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	second, _ := os.ReadFile(p.masterKey)
	if string(second) != string(first) {
		t.Fatal("the master key was replaced on a second bring-up: every secret already sealed under it is now undecryptable")
	}
}

// TestMasterKeyRefusesToGuessAtAnUnusableOne: a file holding something that is not a 32-byte hex key
// makes the control-plane log.Fatalf at boot (identity.ParseMasterKey is a startup error by design).
// Overwriting it would destroy whatever it was; booting on it would take the stack down. Refuse and
// say so instead.
func TestMasterKeyRefusesToGuessAtAnUnusableOne(t *testing.T) {
	p := tempPaths(t)
	if err := os.WriteFile(p.masterKey, []byte("REPLACE_WITH_OPENSSL_RAND_HEX_32"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := ensureMasterKey(p)
	if err == nil {
		t.Fatal("an unusable master key was accepted; the control-plane would refuse to boot on it")
	}
	if !strings.Contains(err.Error(), p.masterKey) {
		t.Fatalf("the refusal must name the file so it can be fixed, got: %v", err)
	}
	after, _ := os.ReadFile(p.masterKey)
	if string(after) != "REPLACE_WITH_OPENSSL_RAND_HEX_32" {
		t.Fatal("the existing master key was overwritten")
	}
}

// TestEverySecretSlotComposeMountsExists: compose bind-mounts each file-secret and a MISSING source
// fails `compose up` outright — which is why `palai init` has always created the provider-one slot
// empty. The master-key slot joins it, and a stack initialised by an earlier build (whose .palai has
// no such file, and whose `init` short-circuits on an existing config.json) must be repaired by the
// bring-up rather than left unable to start.
func TestEverySecretSlotComposeMountsExists(t *testing.T) {
	p := tempPaths(t)
	if err := ensureSecretSlots(p); err != nil {
		t.Fatalf("ensure the secret slots: %v", err)
	}
	for _, path := range []string{p.secretPath("provider-one"), p.masterKey} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("compose mounts %s as a file-secret and it does not exist: %v", filepath.Base(path), err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s is mode %v, want 0600", filepath.Base(path), info.Mode().Perm())
		}
	}
	// A filled slot is never clobbered by a later bring-up.
	if err := os.WriteFile(p.secretPath("provider-one"), []byte("sk-already-here"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureSecretSlots(p); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if got, _ := os.ReadFile(p.secretPath("provider-one")); string(got) != "sk-already-here" {
		t.Fatalf("a stored credential was cleared by the slot ensure: %q", got)
	}
}

// tempPaths builds a paths rooted at a temp dir, so the slot/key helpers can be driven without
// touching the developer's real .palai.
func tempPaths(t *testing.T) paths {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	return paths{
		home:       home,
		secretsDir: filepath.Join(home, "secrets"),
		masterKey:  filepath.Join(home, "secrets", "master-key"),
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
