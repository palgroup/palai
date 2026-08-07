// Package localca mints the trust root a self-hosted control plane serves its runner gateway with:
// one local CA and one gateway server certificate.
//
// IT LIVES HERE, OUTSIDE cmd/cli, BECAUSE THE CONTROL PLANE IS THE ONE THAT CANNOT BOOT WITHOUT IT.
// This code was `cmd/cli/internal/stack/certs.go` and ran only from `palai init`, which made the CLI
// load-bearing for a deployment that otherwise needs nothing from it: `docker compose up -d` on a clean
// machine brought a control plane up that immediately died in `log.Fatalf("load runner server
// certificate")`, because nothing but `init` had ever written one. A trust root that only one optional
// tool can create is a trust root the deployment does not own.
//
// ONE PACKAGE, ONE MINTER. The CLI calls the same Ensure the control plane calls, so a stack initialised
// by either has byte-identical shape — same key type, same SAN, same modes. Two spellings of a CA is how
// a fleet ends up with runners that trust one of them.
package localca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// ControllerDNS is the ONE name the gateway certificate vouches for, and the name a runner pins.
//
// A device reaches the control plane by ADDRESS and verifies it by this NAME (`palai enroll
// --server-name`), so no IP SAN is needed and none is minted: every extra identity on a certificate is
// one more party it can speak for. deploy/compose's native overlay aliases this name to the host
// gateway for the same reason — the name cannot change without re-minting every fleet's trust.
const ControllerDNS = "control-plane"

// Paths are the four files a trust root occupies. The two keys are private (0600); the two
// certificates are public (0644).
type Paths struct {
	CACert     string
	CAKey      string
	ServerCert string
	ServerKey  string
}

// ErrPartial reports a trust root that exists in pieces. See Ensure for why that is refused rather
// than repaired.
var ErrPartial = errors.New("the trust root is incomplete")

// Ensure mints the CA and the gateway server certificate when NONE of the four files exist, leaves a
// COMPLETE set untouched, and REFUSES a partial one. It reports whether this call did the minting.
//
// THE THREE OUTCOMES ARE THREE DIFFERENT FACTS AND COLLAPSING ANY TWO IS A FLEET-WIDE OUTAGE:
//
//   - Complete: every runner in the fleet holds a certificate this CA signed. Re-minting would make all
//     of them untrusted at once, on a boot that looked routine. So a complete set is never touched, and
//     an operator who supplied their own CA gets theirs used unchanged.
//   - None: nothing trusts anything yet, so minting costs nobody. This is the case that makes
//     `docker compose up -d` stand on its own.
//   - Partial: a CA certificate without its key cannot sign the next enrolment; a server certificate
//     without its CA cannot be verified by anyone. Both are recoverable by a human who knows which file
//     was lost, and NEITHER is recoverable by a program that guesses. Minting over the remainder would
//     silently orphan every enrolled runner, which is the loudest failure this package can cause and the
//     quietest one to cause by accident. It refuses and names the files it found.
//
// It creates the parent directory (0700) when it mints, so a fresh volume needs no preparation.
func Ensure(p Paths) (minted bool, err error) {
	present, absent := split(p)
	switch {
	case len(absent) == 0:
		return false, nil
	case len(present) > 0:
		return false, fmt.Errorf("%w: found %v but not %v; restore the missing file or remove the rest deliberately — "+
			"minting over a partial root would invalidate every runner this CA has already enrolled",
			ErrPartial, present, absent)
	}

	if err := os.MkdirAll(filepath.Dir(p.CACert), 0o700); err != nil {
		return false, fmt.Errorf("create the trust-root directory: %w", err)
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return false, fmt.Errorf("generate CA key: %w", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "palai-local-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return false, fmt.Errorf("sign CA certificate: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return false, fmt.Errorf("parse CA certificate: %w", err)
	}

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return false, fmt.Errorf("generate server key: %w", err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: ControllerDNS},
		DNSNames:     []string{ControllerDNS},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		return false, fmt.Errorf("sign server certificate: %w", err)
	}

	// The KEYS land before the certificates they belong to. A crash between the two writes then leaves a
	// key with no certificate, which split() reads as partial and refuses — the same answer a human would
	// give. Writing certificates first would leave a certificate whose key is missing, which reads the
	// same way, so the order is not about which failure is better; it is that a half-written root must
	// never look complete, and it cannot, because every file is written and neither is optional.
	if err := writeKeyPEM(p.CAKey, caKey); err != nil {
		return false, err
	}
	if err := writeCertPEM(p.CACert, caDER); err != nil {
		return false, err
	}
	if err := writeKeyPEM(p.ServerKey, serverKey); err != nil {
		return false, err
	}
	if err := writeCertPEM(p.ServerCert, serverDER); err != nil {
		return false, err
	}
	return true, nil
}

// split reports which of the four files exist and which do not, in a stable order so a refusal message
// reads the same twice.
func split(p Paths) (present, absent []string) {
	for _, path := range []string{p.CAKey, p.CACert, p.ServerKey, p.ServerCert} {
		if _, err := os.Stat(path); err == nil {
			present = append(present, path)
		} else {
			absent = append(absent, path)
		}
	}
	return present, absent
}

// serial returns a random 128-bit certificate serial.
func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		// crypto/rand failing is fatal for the whole boot; a fixed serial keeps the signature call
		// total without inventing a fallible return here.
		return big.NewInt(1)
	}
	return n
}

// writeCertPEM writes a DER certificate as a 0644 PEM (public material).
func writeCertPEM(path string, der []byte) error {
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, block, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// writeKeyPEM writes an EC private key as a 0600 PKCS#8 PEM — the form execution.NewFileCertIssuer and
// tls.LoadX509KeyPair both parse.
func writeKeyPEM(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal EC key: %w", err)
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, block, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
