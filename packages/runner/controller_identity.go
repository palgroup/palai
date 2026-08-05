package runner

import (
	"crypto/tls"
	"errors"
	"fmt"
)

// verifyControllerIdentity is the runner's check that the peer it verified is the controller it was
// configured for, and that the certificate vouches for nobody else. Enrolment, renewal and the lease
// session all use it, so there is ONE statement of the property rather than three copies drifting apart —
// which is the only thing that changed about the rule itself.
//
// ‼️ THE OVER-BROAD REFUSAL IS THE POINT, and tests/conformance/engine names it:
// TestRunnerRejectsOverBroadServerIdentity mints a server certificate carrying the controller's name AND
// `extra.attacker.conformance.palai.test`. That passes Go's hostname check — the controller's name IS
// present — and must still be refused, because a certificate that also speaks for another host is a
// certificate a second party can present. Nothing here weakens that: exactly one name, and it is the one
// this device was told to expect.
//
// ‼️ WHICH IS WHY A DEVICE DIALS AN ADDRESS AND VERIFIES A NAME. The obvious repair for "the URL host is
// 127.0.0.1 and the certificate says control-plane" is to let the certificate carry both. That repair is
// wrong, and trying it is what showed why: a rule loose enough to admit a second identity cannot tell
// `control-plane` from `extra.attacker...` — the conformance test refused the author's own device, which
// is the test doing its job. So the address and the identity stay separate concerns: the connection goes
// to the address (`--url`), the certificate is verified against the name (`--server-name`, defaulting to
// the URL host when they are the same, as they are for any publicly trusted gateway).
func verifyControllerIdentity(state tls.ConnectionState, expected string) error {
	if len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 {
		return errors.New("controller certificate chain was not verified")
	}
	if expected == "" {
		return errors.New("no controller identity was configured, so nothing could be verified against it")
	}
	leaf := state.VerifiedChains[0][0]
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != expected {
		return fmt.Errorf("the controller certificate names %v, not %q alone — a certificate that speaks for anyone else is one somebody else can present", leaf.DNSNames, expected)
	}
	return nil
}
