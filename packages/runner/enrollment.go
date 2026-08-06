package runner

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Identity is the runner's short-lived enrolled identity: the run-scoped client
// certificate and the private key that never leaves the runner. It deliberately
// carries no enrollment token — the bootstrap credential is presented during Enroll
// and discarded, never retained. It is not one-use (the control plane admits one
// redemption per issued-certificate lifetime), which makes NOT retaining it the
// stronger property: the runner re-reads it from disk when it needs it.
type Identity struct {
	RunnerID    string
	Certificate tls.Certificate
	NotAfter    time.Time
	// Settings is the configuration the control plane sent with this identity, nil when it sent none. It is
	// carried on the identity rather than returned beside it because the two answer one question a machine
	// asks once — who am I, and what am I — and splitting them lets a caller take the certificate and forget
	// the configuration.
	Settings map[string]string
}

// BootstrapConfig is the enrollment input. EnrollmentToken is presented and never
// stored — not one-use, but not retained either; ControllerCAs and ControllerDNS are the trust anchor and exact
// server identity the runner requires for every outbound connection.
type BootstrapConfig struct {
	RunnerID        string
	RunnerDNS       string
	EnrollmentToken string
	EnrollmentURL   string
	// ControllerCAs is the ADDITIONAL trust anchor for a private deployment, and NIL IS NOW A NORMAL
	// VALUE rather than a refusal: a publicly trusted runner gateway is verified by the host's own root
	// store, which is DoD item 2 and the reason `PALAI_CONTROLLER_CA` stopped being a required input.
	//
	// Nil means "the system roots decide". A non-nil pool means "these roots and only these", which is
	// what every deployment built before this sent and what a self-signed self-host still needs.
	ControllerCAs *x509.CertPool
	ControllerDNS string
	Now           func() time.Time
	// Posture is what this machine SAYS it is: "sandboxed-linux" (a container runner) or
	// "unsandboxed-host" (a rented Mac). The control plane compares it with the pool's and refuses the
	// enrolment on a disagreement (E24 T2) — it does NOT verify it, and cannot.
	//
	// Empty declares nothing, which is what every runner built before E24 sends and is why this field
	// is omitempty below: an undeclared machine's request body is byte-identical to the one this
	// package has always sent, so no deployment observes the field's existence.
	Posture string
	// PoolID is the pool this machine was CONFIGURED to join (E24 T3). It is a declaration, not an
	// authorisation: the pool a certificate is actually minted into comes from the presented KEY, and a
	// declaration that disagrees with the key REFUSES the enrolment instead of being silently widened
	// or silently overridden.
	//
	// Why declare it at all, when the key already decides: because the realistic operator mistake is
	// pasting the wrong pool's key onto a machine, and posture comparison (T2) cannot catch it when
	// both pools have the same posture. Saying where the machine believes it belongs is what turns that
	// into a refusal at the door instead of a machine quietly running another pool's work.
	//
	// Empty declares nothing and inherits the key's pool, which is what every runner built before E24
	// sends — hence omitempty, so an unconfigured machine's request body is unchanged to the byte.
	PoolID string
	// Capacity is how many sessions this machine can hold AT ONCE (Faz A.4 T5). It is a declaration in the
	// same sense Posture is — the control plane records it and enforces it, and cannot verify it.
	//
	// ZERO DECLARES NOTHING AND NO CEILING IS ENFORCED, which is the shipped posture and not a fallback.
	// Before this field the column existed, refused zero, and was filled by a clamp in the control plane:
	// every machine in every deployment carried a 1 that no operator had chosen. Enforcing that would have
	// capped every Mac at one session on the strength of an artefact. So the ceiling exists only where
	// somebody typed a number, and an unconfigured machine's request body is unchanged to the byte.
	Capacity int
	// DeviceKey is the machine's DURABLE keypair (packages/device). When it is set, Enroll signs a CSR
	// with it and generates nothing — which is the whole of "a restart is the same machine": the
	// registry keys a row on this key's fingerprint, so the id follows the key and the key follows the
	// disk (plan §3.4).
	//
	// NIL KEEPS THE OLD BEHAVIOUR — a fresh keypair per call, no CSR, the raw public key on the wire —
	// because every control plane and every test built before this sends exactly that, and a package
	// that could only enrol the new way would be a package no existing deployment can use.
	DeviceKey crypto.Signer
	// RecoverRunnerID is the id this machine ALREADY HOLDS, read from its own disk, presented so the
	// control plane reissues that identity instead of minting a second one. Empty asks for a new
	// identity, which is what a machine enrolling for the first time is doing.
	//
	// IT AUTHORISES NOTHING. The fingerprint of DeviceKey is what proves who this machine is; this field
	// is a claim, and a claim that disagrees with the fingerprint is REFUSED rather than honoured — that
	// is what stops a device holding a stolen identity file from becoming the machine it names.
	RecoverRunnerID string
	// The MEASURED facts (plan §3.3, DoD 9): what this binary was compiled for, what version it is, and
	// which session-isolation modes this machine can actually provide. The gateway checks them against
	// the key's pool BEFORE issuing an identity, so a machine that cannot execute never becomes ready
	// capacity. All are omitempty on the wire: a runner that measures nothing sends the bytes it has
	// always sent.
	OS             string
	Arch           string
	Version        string
	IsolationModes []string
}

type enrollmentRequest struct {
	RunnerID  string `json:"runner_id"`
	PublicKey string `json:"public_key"`
	Posture   string `json:"posture,omitempty"`
	PoolID    string `json:"pool_id,omitempty"`
	// Capacity is how many sessions this machine says it can hold at once (Faz A.4 T5). Zero declares
	// NOTHING and `omitempty` keeps it off the wire entirely, so a machine that was not configured with one
	// sends the same bytes it always sent — the same rule Posture and PoolID follow above, and the reason
	// the placement ceiling is opt-in rather than a number the control plane invents.
	Capacity int `json:"capacity,omitempty"`
	// CSR is the base64 PKCS#10 whose SIGNATURE proves this machine holds the private half of the key it
	// is asking to have certified. Before it, the enrolment sent a bare public key and the comment ten
	// lines below said so — "no CSR proof-of-possession" — which meant a party holding a pool key could
	// enrol ANY public key, including one whose private half belonged to somebody else.
	//
	// `omitempty` and the PublicKey field above coexist deliberately: a control plane too old to verify a
	// CSR reads PublicKey, and a runner too old to send one is still accepted. The gateway prefers the
	// CSR when both are present and refuses a CSR whose signature does not check.
	CSR string `json:"csr,omitempty"`
	// RecoverRunnerID is the identity this machine already holds. It is a CLAIM checked against the
	// fingerprint, never an authorisation — see BootstrapConfig.RecoverRunnerID.
	RecoverRunnerID string `json:"recover_runner_id,omitempty"`
	// The measured facts. OS and Arch have been on this wire since E24 (the gateway's enrollRequest
	// declares them); Version and IsolationModes are new and are what let the gateway refuse a machine
	// that cannot execute before it becomes ready capacity.
	OS             string   `json:"os,omitempty"`
	Arch           string   `json:"arch,omitempty"`
	Version        string   `json:"version,omitempty"`
	IsolationModes []string `json:"isolation_modes,omitempty"`
}

type enrollmentResponse struct {
	Certificate string `json:"certificate"`
	// Settings is this machine's configuration as the admin plane decided it — the pool's desired document.
	// It arrives HERE rather than from a file on the machine, which is the point: an operator configures a
	// fleet from one screen instead of editing an env file on every box.
	//
	// Absent means "no document for this pool", not "an empty configuration": the runner then uses what it
	// was started with, which is what every runner did before this field existed.
	Settings map[string]string `json:"settings,omitempty"`
	// RunnerID is the id the CONTROL PLANE minted (E24 T1). The runner_id this runner sent is a label
	// — the compose entrypoint hardcodes it, so it is not unique across machines — and the server's is
	// the one that names this machine everywhere else. A control plane too old to send it leaves this
	// empty and the runner keeps its own name, which is what makes an old control plane still work.
	RunnerID string `json:"runner_id"`
}

// Enroll exchanges the bootstrap credential for a short-lived client identity over
// an outbound, server-authenticated TLS connection. The runner generates its own
// keypair locally; only the public key is sent, and the returned certificate plus that
// local key form the identity. The token is presented and discarded — it is not one-use, so a
// machine whose certificate expired may re-present it, but nothing here retains it.
func Enroll(ctx context.Context, config BootstrapConfig) (Identity, error) {
	// ControllerCAs IS NO LONGER IN THIS LIST. Nil now means "verify against the host's own root store",
	// which is what a publicly trusted runner gateway needs and what DoD item 2 makes the default; a
	// private deployment still passes a pool and still gets exactly that pool. Requiring it was the last
	// thing that made a CA file a mandatory input on every device.
	if config.RunnerID == "" || config.RunnerDNS == "" || config.EnrollmentToken == "" ||
		config.ControllerDNS == "" || config.Now == nil {
		return Identity{}, errors.New("enrollment requires runner identity, bootstrap token, controller DNS and clock")
	}
	if !strings.HasPrefix(config.EnrollmentURL, "https://") {
		return Identity{}, errors.New("enrollment URL must be outbound https")
	}

	// THE KEY IS THE CALLER'S WHEN THE CALLER HAS ONE. A device passes its durable key and this function
	// generates nothing, so the fingerprint the registry keys on is the same on every start. Without one
	// it mints a fresh key per call, which is the pre-device behaviour every existing deployment relies on
	// — and which is exactly why a machine used to arrive as a new row after every reboot.
	privateKey, err := enrollmentKey(config)
	if err != nil {
		return Identity{}, err
	}
	publicDER, err := x509.MarshalPKIXPublicKey(privateKey.Public())
	if err != nil {
		return Identity{}, fmt.Errorf("marshal runner public key: %w", err)
	}

	// PROOF OF POSSESSION, which this request had none of until now. The comment that stood here said
	// so — "ponytail: no CSR proof-of-possession" — and the consequence was concrete: anyone holding a
	// pool key could have a certificate minted for a public key whose private half was somebody else's.
	// A device signs a PKCS#10 with its durable key and the gateway checks the signature; a caller with
	// no device key sends none and the gateway falls back to the bare public key, so no existing
	// deployment's request body changes.
	var csr string
	if config.DeviceKey != nil {
		der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
			Subject:            pkix.Name{CommonName: config.RunnerID},
			SignatureAlgorithm: x509.ECDSAWithSHA256,
		}, config.DeviceKey)
		if err != nil {
			return Identity{}, fmt.Errorf("build certificate request: %w", err)
		}
		csr = base64.StdEncoding.EncodeToString(der)
	}

	body, err := json.Marshal(enrollmentRequest{
		RunnerID:        config.RunnerID,
		PublicKey:       base64.StdEncoding.EncodeToString(publicDER),
		Posture:         config.Posture,
		PoolID:          config.PoolID,
		Capacity:        config.Capacity,
		CSR:             csr,
		RecoverRunnerID: config.RecoverRunnerID,
		OS:              config.OS,
		Arch:            config.Arch,
		Version:         config.Version,
		IsolationModes:  config.IsolationModes,
	})
	if err != nil {
		return Identity{}, fmt.Errorf("encode enrollment request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, config.EnrollmentURL, bytes.NewReader(body))
	if err != nil {
		return Identity{}, fmt.Errorf("build enrollment request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+config.EnrollmentToken)
	request.Header.Set("Content-Type", "application/json")

	transport := &http.Transport{TLSClientConfig: enrollmentTLS(config), Proxy: nil}
	defer transport.CloseIdleConnections()
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return Identity{}, fmt.Errorf("enroll with control plane: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		// ‼️ THE REASON IS IN THE BODY AND THIS USED TO THROW IT AWAY. The gateway writes a distinct
		// sentence for every refusal — "declared posture is not this pool's", "this machine does not
		// support the isolation mode this pool requires", "the claimed runner identity does not belong to
		// this device key" — and printing only `response.Status` reduced all of them to "409 Conflict".
		// The person reading it is standing at the machine with a key file in their hand; the difference
		// between those three sentences is the difference between minting another key, choosing another
		// pool, and re-imaging the box. Measured 2026-08-06: three rounds of guessing on a real enrolment
		// that the server had already explained.
		//
		// Bounded and trimmed, because it is server-controlled text going into a local error string.
		reason, _ := io.ReadAll(io.LimitReader(response.Body, 4*1024))
		if detail := strings.TrimSpace(string(reason)); detail != "" {
			return Identity{}, fmt.Errorf("enrollment rejected: %s: %s", response.Status, detail)
		}
		return Identity{}, fmt.Errorf("enrollment rejected: %s", response.Status)
	}

	var decoded enrollmentResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&decoded); err != nil {
		return Identity{}, fmt.Errorf("decode enrollment response: %w", err)
	}
	certDER, err := base64.StdEncoding.DecodeString(decoded.Certificate)
	if err != nil {
		return Identity{}, fmt.Errorf("decode issued certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		return Identity{}, fmt.Errorf("parse issued certificate: %w", err)
	}
	if !leaf.NotAfter.After(config.Now()) {
		return Identity{}, errors.New("control plane issued an already-expired certificate")
	}

	// Adopt the server's id when it sent one. Falling back to the requested name is not a courtesy —
	// it is what lets this runner enroll against a control plane built before E24 T1.
	runnerID := decoded.RunnerID
	if runnerID == "" {
		runnerID = config.RunnerID
	}
	return Identity{
		RunnerID:    runnerID,
		Certificate: tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: privateKey, Leaf: leaf},
		NotAfter:    leaf.NotAfter,
		Settings:    decoded.Settings,
	}, nil
}

// enrollmentKey answers which private key this enrolment certifies: the device's durable one when the
// caller has it, a fresh one otherwise.
//
// THE FRESH BRANCH IS THE OLD BEHAVIOUR AND IT IS NOT DEAD. Every control plane test, the conformance
// harness and every deployment that has not been re-enrolled with `palai enroll` reaches it, and each
// of those genuinely wants a per-process key. What made it a defect was being the ONLY branch.
func enrollmentKey(config BootstrapConfig) (crypto.Signer, error) {
	if config.DeviceKey != nil {
		return config.DeviceKey, nil
	}
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate runner key: %w", err)
	}
	return private, nil
}

// enrollmentTLS is the outbound, server-authenticated TLS config for enrollment: the
// runner presents no client certificate (it has none yet — the token authenticates it)
// but pins the exact DNS identity, against the controller CA when one was given and against
// the host's own root store when none was.
//
// ‼️ A NIL RootCAs IS NOT "TRUST NOTHING" AND IT IS NOT "TRUST ANYTHING" — crypto/tls reads it as
// "use the host's root store", which is the behaviour a publicly trusted gateway needs and the reason a
// CA file stopped being a required device input. InsecureSkipVerify is never set on either branch, so
// both paths still verify a chain and both still run the exact-DNS check below.
//
// THE EXACT-DNS CHECK SURVIVES THE CHANGE, AND ON THE PUBLIC BRANCH IT IS STRICTER THAN THE WEB'S. A
// publicly trusted certificate routinely carries several SANs (apex, www, a wildcard); this requires
// exactly one and requires it to be the configured name. That is deliberate for a runner gateway — the
// device is pointed at one hostname by its own config, not by a link somebody clicked — and it is
// written down here because it is the kind of rule that looks like a bug to whoever first points a
// device at a multi-SAN certificate.
func enrollmentTLS(config BootstrapConfig) *tls.Config {
	now := config.Now
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: config.ControllerDNS,
		Time:       func() time.Time { return now() },
	}
	if config.ControllerCAs != nil {
		tlsConfig.RootCAs = config.ControllerCAs.Clone()
	}
	tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
		return verifyControllerIdentity(state, config.ControllerDNS)
	}
	return tlsConfig
}
