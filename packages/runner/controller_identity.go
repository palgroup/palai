package runner

import (
	"crypto/tls"
	"errors"
	"fmt"
)

// verifyControllerIdentity is the runner's check that the peer it verified is the controller it was
// configured for. Enrolment, renewal and the lease session all use it, so there is ONE statement of the
// property rather than three copies drifting apart.
//
// ‼️ IT REPLACED A PIN THAT COULD NOT SURVIVE AN ADDRESS. The three call sites each asserted
// `len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != host`: the certificate had to carry exactly one DNS
// name and it had to equal the host from the controller URL. That holds only when a device is handed a
// hostname AND the certificate names nothing else. It fails outright when a device is handed an
// ADDRESS — measured 2026-08-06 on the real enrol path, where the URL host was `127.0.0.1`, the
// certificate named `control-plane`, and every attempt answered "controller certificate DNS identity is
// not exact" no matter how the certificate was minted. It also fails a publicly trusted gateway, whose
// certificate legitimately carries several names, which is the deployment the device plan is built for.
//
// WHAT IS KEPT IS THE PROPERTY, NOT THE ARITHMETIC. VerifyHostname answers the question the pin was
// trying to ask — is this certificate valid for the host I was told to reach — against DNS names and IP
// addresses alike. The chain was already verified against the configured roots before this runs, and
// mTLS still governs who may connect at all; what this adds is that a certificate the CA signed for some
// OTHER host cannot stand in for the controller.
func verifyControllerIdentity(state tls.ConnectionState, host string) error {
	if len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 {
		return errors.New("controller certificate chain was not verified")
	}
	if host == "" {
		return errors.New("no controller host was configured, so nothing could be verified against it")
	}
	if err := state.VerifiedChains[0][0].VerifyHostname(host); err != nil {
		return fmt.Errorf("the controller certificate is not valid for %s: %w", host, err)
	}
	return nil
}
