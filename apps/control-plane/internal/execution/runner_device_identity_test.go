package execution_test

// The device-identity proofs (plan §3.4, T2). They drive the REAL gateway over REAL TLS with a REAL
// packages/device key, because every claim here is about what the WIRE does: whether a CSR is verified,
// whether a restart is one machine, and whether a claimed identity can be taken by a machine that cannot
// prove it.
//
// WHAT THESE DO *NOT* PROVE, said before the first test rather than after the last: they do not prove
// what fleet.Store does. The registry behind them is the in-memory fake, which restates the CONTRACT
// (the error values fleet exports) and not the store's implementation. The store's own behaviour —
// including the 000007 unique index that decides the concurrent case — is measured against a real
// Postgres in the fleet package's component suite.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/fleet"
	"github.com/palgroup/palai/packages/device"
	"github.com/palgroup/palai/packages/runner"
)

// deviceBootstrap is f.bootstrap plus a durable device key and the facts a real agent measures, so a
// proof drives the same request `palai enroll` sends rather than a shape only this file produces.
func deviceBootstrap(t *testing.T, f *gatewayFixture, token string, key device.DeviceKey, holdID string) runner.BootstrapConfig {
	t.Helper()
	config := f.bootstrap(token)
	config.DeviceKey = key.Signer()
	config.RecoverRunnerID = holdID
	config.OS, config.Arch, config.Version = "darwin", "arm64", "9.9.9"
	config.IsolationModes = []string{device.IsolationUser}
	return config
}

func newDeviceKey(t *testing.T) device.DeviceKey {
	t.Helper()
	key, err := device.LoadOrCreateDeviceKey(filepath.Join(t.TempDir(), "device.key"))
	if err != nil {
		t.Fatalf("device key: %v", err)
	}
	return key
}

// TestARestartIsOneMachineAndNotASecondRow is plan §T2's second RED, and the defect it closes was
// measured on this tree: `packages/runner.Enroll` generated a fresh keypair on every call and
// `cmd/runner` called it once per PROCESS START, so a machine that rebooted arrived as a machine the
// registry had never seen — new id, new row, and in a strict pool a second approval for a box a human
// had already approved.
//
// ‼️ THE ASSERTION IS ON THE CERTIFICATE AS WELL AS ON THE ROW COUNT, and that pairing is what makes it
// mean anything. A registry that recovered the row while the gateway still signed its freshly minted id
// would produce ONE row and a certificate naming a machine no row records — the exact identity/DNS split
// this package's registry test already caught once, arriving one layer later. So: one row, one id, and
// the SAN of every certificate is that id's.
func TestARestartIsOneMachineAndNotASecondRow(t *testing.T) {
	f := newGatewayFixture(t, newOneUseTokens("t1", "t2", "t3", "t4"))
	registry := newFakeRegistry()
	f.gateway.SetRegistry(registry)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	key := newDeviceKey(t)
	first, err := runner.Enroll(ctx, deviceBootstrap(t, f, "t1", key, ""))
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	if !strings.HasPrefix(first.RunnerID, "rnr_") {
		t.Fatalf("the server did not mint the id: %q", first.RunnerID)
	}

	// Three more starts, which is plan §T2's "kill and restart the agent three times". Each one presents
	// the SAME device key and the id it holds, exactly as an installed agent does.
	for i, token := range []string{"t2", "t3", "t4"} {
		again, err := runner.Enroll(ctx, deviceBootstrap(t, f, token, key, first.RunnerID))
		if err != nil {
			t.Fatalf("restart %d: %v", i+1, err)
		}
		if again.RunnerID != first.RunnerID {
			t.Fatalf("restart %d came back as %q, want the machine's own id %q — every reboot would be a new machine",
				i+1, again.RunnerID, first.RunnerID)
		}
		if got := certDNS(t, again); got != runnerDNSFor(first.RunnerID) {
			t.Fatalf("restart %d was issued a certificate for %q while its row is %q: the certificate and the row "+
				"name different machines, and every later lookup goes through the certificate", i+1, got, first.RunnerID)
		}
	}

	rows := registry.snapshot()
	if len(rows) != 1 {
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		t.Fatalf("four starts of one machine produced %d rows (%v), want 1", len(rows), ids)
	}
	if rows[0].PublicKeySHA256 != key.Fingerprint() {
		t.Fatalf("the row is keyed on %q, not on this device's key %q", rows[0].PublicKeySHA256, key.Fingerprint())
	}
}

// TestANewKeyIsANewMachineAndNeverAnExistingOne is the other side of the same rule, and without it the
// test above would be satisfied by a gateway that returned the first machine's identity to EVERYBODY.
// That is not a hypothetical failure mode — it is what "recover by fingerprint" degrades into if the
// fingerprint is not actually consulted.
func TestANewKeyIsANewMachineAndNeverAnExistingOne(t *testing.T) {
	f := newGatewayFixture(t, newOneUseTokens("t1", "t2"))
	registry := newFakeRegistry()
	f.gateway.SetRegistry(registry)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	first, err := runner.Enroll(ctx, deviceBootstrap(t, f, "t1", newDeviceKey(t), ""))
	if err != nil {
		t.Fatalf("first machine: %v", err)
	}
	second, err := runner.Enroll(ctx, deviceBootstrap(t, f, "t2", newDeviceKey(t), ""))
	if err != nil {
		t.Fatalf("second machine: %v", err)
	}
	if first.RunnerID == second.RunnerID {
		t.Fatalf("two machines with two device keys share id %q: a genuinely new install stole an existing identity", first.RunnerID)
	}
	if len(registry.snapshot()) != 2 {
		t.Fatalf("two machines produced %d rows", len(registry.snapshot()))
	}
}

// TestAClaimedIdentityIsCheckedAgainstTheFingerprint is plan §T2's third RED, in the two shapes that
// reach it.
//
// ‼️ WHY THE CLAIM MUST NOT SIMPLY BE IGNORED. Ignoring it and minting a fresh id is the friendly
// answer and the wrong one: on the honest shape (a re-image that kept identity.json and lost
// device.key) it hides that the machine believes it is something it cannot prove, and on the hostile
// shape it is exactly the request an attacker makes after copying one small JSON file off a box.
func TestAClaimedIdentityIsCheckedAgainstTheFingerprint(t *testing.T) {
	f := newGatewayFixture(t, newOneUseTokens("t1", "t2", "t3"))
	registry := newFakeRegistry()
	f.gateway.SetRegistry(registry)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	victimKey := newDeviceKey(t)
	victim, err := runner.Enroll(ctx, deviceBootstrap(t, f, "t1", victimKey, ""))
	if err != nil {
		t.Fatalf("the machine being impersonated could not enrol: %v", err)
	}

	// SHAPE ONE — another machine, its own key, claiming the victim's id. This is the theft.
	if _, err := runner.Enroll(ctx, deviceBootstrap(t, f, "t2", newDeviceKey(t), victim.RunnerID)); err == nil {
		t.Fatal("a machine holding a different device key was issued the identity it merely NAMED: an identity file " +
			"copied off a box would be an identity")
	}

	// SHAPE TWO — the honest re-image: no row anywhere carries this fingerprint, and the machine still
	// names an id. Refused for the same reason, because the server cannot tell the two apart.
	if _, err := runner.Enroll(ctx, deviceBootstrap(t, f, "t3", newDeviceKey(t), "rnr_never_existed")); err == nil {
		t.Fatal("a machine claiming an id no fingerprint supports was admitted")
	}

	// The victim's row is untouched: a refused theft must not have written anything.
	rows := registry.snapshot()
	if len(rows) != 1 || rows[0].ID != victim.RunnerID {
		t.Fatalf("the refused enrolments left %d rows: %+v", len(rows), rows)
	}
}

// TestARevokedDeviceCannotComeBackWithALivePoolKey is the fourth refusal, and it is the one a reusable
// pool key makes necessary.
//
// ‼️ A POOL KEY ENROLS A FLEET, NOT A BOX — it is reusable by design (fleet.keys.go). So before device
// keys, "revoke that Mac" was undone by the Mac restarting: it enrolled again under a NEW id with the
// key still sitting on its disk, and the revocation named a row nothing would ever present again. The
// fingerprint is what the revocation actually named, so presenting it now recovers the revoked row and
// is refused.
func TestARevokedDeviceCannotComeBackWithALivePoolKey(t *testing.T) {
	f := newGatewayFixture(t, newOneUseTokens("t1", "t2"))
	registry := newFakeRegistry()
	f.gateway.SetRegistry(registry)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	key := newDeviceKey(t)
	enrolled, err := runner.Enroll(ctx, deviceBootstrap(t, f, "t1", key, ""))
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	registry.revoke(enrolled.RunnerID)

	if _, err := runner.Enroll(ctx, deviceBootstrap(t, f, "t2", key, enrolled.RunnerID)); err == nil {
		t.Fatal("a revoked machine re-enrolled with a live pool key: a decommissioning that the machine can undo " +
			"by restarting is not a decommissioning")
	}
	if rows := registry.snapshot(); len(rows) != 1 {
		t.Fatalf("the refused re-enrolment produced %d rows, want the one revoked row", len(rows))
	}
}

// TestAnUnverifiableCSRIsRefusedRatherThanDowngraded is the proof-of-possession gate, and the word that
// carries it is RATHER THAN.
//
// ‼️ THE FALLBACK IS THE WHOLE RISK. `public_key` is still accepted when no CSR is sent — that is the
// compatibility path every runner built before packages/device takes. If a CSR that fails verification
// ALSO fell back to it, an attacker wanting a certificate for a key it does not hold would corrupt one
// byte of the signature and get the pre-CSR behaviour back. So the three legs are: a good CSR is
// accepted, a corrupted one is REFUSED, and a request with no CSR at all still works.
//
// It drives raw HTTP rather than packages/runner because packages/runner cannot produce a broken CSR —
// which is the point: the refusal has to hold against a client this repository did not write.
func TestAnUnverifiableCSRIsRefusedRatherThanDowngraded(t *testing.T) {
	f := newGatewayFixture(t, newOneUseTokens("good", "corrupt", "legacy"))
	f.gateway.SetRegistry(newFakeRegistry())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	key := newDeviceKey(t)
	csr, err := key.CertificateRequest("some-mac")
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(key.Signer().Public())
	if err != nil {
		t.Fatal(err)
	}
	corrupted := append([]byte(nil), csr...)
	corrupted[len(corrupted)-1] ^= 0xff

	for _, tc := range []struct {
		name     string
		token    string
		body     map[string]any
		wantCode int
	}{
		{
			name: "a signed request is accepted", token: "good", wantCode: http.StatusOK,
			body: map[string]any{"runner_id": "mac-1", "csr": base64.StdEncoding.EncodeToString(csr)},
		},
		{
			// The public key in the body is the REAL one and would have been accepted on its own; only
			// the CSR's signature is broken. A gateway that fell back would answer 200 here.
			name:  "a corrupted signature is refused and does not fall back to the public key beside it",
			token: "corrupt", wantCode: http.StatusBadRequest,
			body: map[string]any{
				"runner_id":  "mac-2",
				"csr":        base64.StdEncoding.EncodeToString(corrupted),
				"public_key": base64.StdEncoding.EncodeToString(publicDER),
			},
		},
		{
			name: "a runner too old to send a CSR still enrols", token: "legacy", wantCode: http.StatusOK,
			body: map[string]any{"runner_id": "mac-3", "public_key": base64.StdEncoding.EncodeToString(publicDER)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := postEnroll(t, ctx, f, tc.token, tc.body)
			if code != tc.wantCode {
				t.Fatalf("status = %d, want %d: %s", code, tc.wantCode, body)
			}
		})
	}
}

// TestAPoolCanRequireAnIsolationModeAMachineMustHaveMeasured is DoD 9 on the wire: the gateway checks
// the machine's MEASURED modes against the key's pool BEFORE issuing an identity, so a machine that
// cannot execute never becomes ready capacity.
//
// THE NON-VACUITY LEG IS THE SECOND ONE. A pool that requires nothing — which is every pool that exists
// the day 000007 applies — must admit a machine that measured nothing, or this check refuses every
// deployment in existence for a mechanism none of them declared.
func TestAPoolCanRequireAnIsolationModeAMachineMustHaveMeasured(t *testing.T) {
	f := newGatewayFixture(t, newOneUseTokens("t1", "t2"))
	registry := newFakeRegistry()
	registry.requireIsolation(fleet.DefaultPoolID, device.IsolationAccounts)
	f.gateway.SetRegistry(registry)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The machine measured `user` only: no palai-agentd, so it cannot give each session its own account.
	config := deviceBootstrap(t, f, "t1", newDeviceKey(t), "")
	if _, err := runner.Enroll(ctx, config); err == nil {
		t.Fatal("a machine that measured no `accounts` isolation joined an accounts-only pool: DoD 19 makes a " +
			"multi-tenant pool accounts-only, and this is the line that enforces it")
	}
	if rows := registry.snapshot(); len(rows) != 0 {
		t.Fatalf("the refused machine was recorded as capacity anyway: %+v", rows)
	}

	// The same machine, having measured the mode, is admitted.
	config = deviceBootstrap(t, f, "t2", newDeviceKey(t), "")
	config.IsolationModes = []string{device.IsolationAccounts, device.IsolationUser}
	if _, err := runner.Enroll(ctx, config); err != nil {
		t.Fatalf("a machine that measured the pool's mode was refused: %v", err)
	}
}

// TestAPublicRootStoreNeedsNoCAFile is plan §T2's first RED, in the form this repository can measure
// without owning a public certificate: a bootstrap with NO CA pool at all must reach the host's root
// store rather than a nil-pointer or a pool that trusts nothing.
//
// ‼️ HONEST CEILING, AND IT IS THE POINT OF THIS TEST RATHER THAN A FOOTNOTE. "Succeeds against a
// publicly trusted server" is NOT measured here and cannot be: this suite's gateway is signed by a
// throwaway CA that no root store contains, and installing one into the machine's trust store is not
// something a unit test may do. What IS measured is the two halves that failed before: nil no longer
// REFUSES at the config check (it did — packages/runner's Enroll listed ControllerCAs in its required
// fields), and nil reaches a real verification rather than skipping one, which the refusal below proves
// by being a certificate error. The publicly-trusted leg belongs to Milestone A0, on a machine with a
// real name.
func TestAPublicRootStoreNeedsNoCAFile(t *testing.T) {
	f := newGatewayFixture(t, newOneUseTokens("t1"))
	f.gateway.SetRegistry(newFakeRegistry())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	config := deviceBootstrap(t, f, "t1", newDeviceKey(t), "")
	config.ControllerCAs = nil

	_, err := runner.Enroll(ctx, config)
	if err == nil {
		t.Fatal("a gateway signed by a throwaway CA was accepted against the system roots: nil must mean " +
			"'the host's root store decides', never 'skip verification'")
	}
	// The refusal has to be about the CERTIFICATE. Anything else — a missing-field error, a nil
	// dereference — would mean nil was rejected before the handshake, which is the behaviour being removed.
	message := err.Error()
	if strings.Contains(message, "enrollment requires") {
		t.Fatalf("nil CA was refused by the config check rather than by verification: %v\n"+
			"that check is what made a CA file a required input on every device", err)
	}
	if !strings.Contains(message, "certificate") && !strings.Contains(message, "authority") && !strings.Contains(message, "tls") {
		t.Fatalf("a nil CA pool failed for a reason that is not certificate verification: %v", err)
	}

	// AND THE POSITIVE HALF: the same enrolment, with the private CA explicitly given, succeeds. Without
	// this leg the assertion above is satisfied by an enrolment that can never work at all.
	if _, err := runner.Enroll(ctx, deviceBootstrap(t, f, "t1", newDeviceKey(t), "")); err != nil {
		t.Fatalf("the private-CA path stopped working: %v", err)
	}
}

// enrollClient is an HTTP client that trusts this fixture's CA and pins its DNS name — the same trust
// packages/runner establishes, restated here because these proofs need to send bodies packages/runner
// cannot build.
func (f *gatewayFixture) enrollClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			RootCAs:    f.ca.pool,
			ServerName: gwControllerDNS,
		},
		Proxy: nil,
	}}
}

// postEnroll drives the enrolment route with a hand-built body, for the cases packages/runner cannot
// produce. It returns the status and the body so a refusal can be read rather than guessed at.
func postEnroll(t *testing.T, ctx context.Context, f *gatewayFixture, token string, body map[string]any) (int, string) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, f.enrollURL, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := f.enrollClient().Do(request)
	if err != nil {
		t.Fatalf("post enroll: %v", err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 8*1024))
	return response.StatusCode, strings.TrimSpace(string(raw))
}
