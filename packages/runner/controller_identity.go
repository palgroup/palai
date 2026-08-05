package runner

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
)

// verifyControllerIdentity is the runner's check that the peer it verified is the controller it was
// configured for, AND that the certificate vouches for nobody else. Enrolment, renewal and the lease
// session all use it, so there is ONE statement of the property rather than three copies drifting apart.
//
// ‼️ THE OVER-BROAD REFUSAL IS THE POINT, and tests/conformance/engine names it:
// TestRunnerRejectsOverBroadServerIdentity mints a server certificate carrying the controller's name AND
// `extra.attacker.conformance.palai.test`. That passes Go's hostname check — the controller's name IS
// present — and must still be refused, because a certificate that also speaks for another host is a
// certificate a second party can present.
//
// ‼️ AND A LOOPBACK ADDRESS IS NOT ANOTHER PARTY, which is the whole of the change here. The three call
// sites used to assert `len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != host`: exactly one name, equal to
// the configured host. That cannot hold for a device handed an ADDRESS — measured 2026-08-06 on the real
// enrol path, where the URL host was `127.0.0.1` and the certificate named `control-plane` — and it
// cannot hold for a publicly trusted gateway, whose certificate legitimately carries several names.
//
// So the rule is narrower than "one name" and stricter than "valid for this host": the certificate must
// be valid for the configured host, every DNS name it carries must BE that host, and any IP address it
// carries must be loopback. 127.0.0.1 and ::1 designate the machine doing the dialling and can never
// designate somebody else, so admitting them widens nothing; admitting a second DNS name would.
func verifyControllerIdentity(state tls.ConnectionState, host string) error {
	if len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 {
		return errors.New("controller certificate chain was not verified")
	}
	if host == "" {
		return errors.New("no controller host was configured, so nothing could be verified against it")
	}
	leaf := state.VerifiedChains[0][0]
	if err := leaf.VerifyHostname(host); err != nil {
		return fmt.Errorf("the controller certificate is not valid for %s: %w", host, err)
	}
	if err := refuseOverBroadIdentity(leaf, host); err != nil {
		return err
	}
	return nil
}

// refuseOverBroadIdentity rejects any identity on the certificate that is not the configured host or a
// loopback address. It is separate so the reason a name was refused can name the name.
func refuseOverBroadIdentity(leaf *x509.Certificate, host string) error {
	for _, name := range leaf.DNSNames {
		if name != host {
			return fmt.Errorf("the controller certificate also vouches for %q, so it is not this controller's alone", name)
		}
	}
	for _, ip := range leaf.IPAddresses {
		if !ip.IsLoopback() {
			return fmt.Errorf("the controller certificate also vouches for %s, which is not loopback and not this controller's alone", ip)
		}
	}
	if len(leaf.DNSNames) == 0 && len(leaf.IPAddresses) == 0 {
		return errors.New("the controller certificate carries no subject alternative name")
	}
	return nil
}
