package stack

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"
)

// writeLocalCA generates the local control-plane CA and the runner-gateway server
// certificate `init` writes into .palai/ca. The CA (PKCS#8 EC key) is what
// execution.NewFileCertIssuer loads to sign enrolling runners; the server certificate
// carries exactly one DNS name — controllerDNS — because the runner pins that identity
// exactly (packages/runner), plus loopback IP SANs so a device handed an address can
// verify it. Keys are 0600; the public certs are 0644.
func writeLocalCA(p paths) error {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate CA key: %w", err)
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
		return fmt.Errorf("sign CA certificate: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return fmt.Errorf("parse CA certificate: %w", err)
	}

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate server key: %w", err)
	}
	// LOOPBACK IP SANs, BECAUSE A DEVICE IS GIVEN AN ADDRESS AND NOT A COMPOSE HOSTNAME. The DNS name
	// alone is what a container on the compose network resolves; an agent installed on the host — which
	// is the whole point of the device contract — is handed a URL, and the only address that is always
	// correct for a self-host plane on this machine is loopback.
	//
	// Measured 2026-08-06 without these: `palai enroll --url https://127.0.0.1:<gateway>` fails with
	// "x509: cannot validate certificate for 127.0.0.1 because it doesn't contain any IP SANs", and the
	// operator's only cures were editing /etc/hosts (root) or re-minting the certificate by hand.
	//
	// It is the SERVER certificate only. The runner's client certificate and the CA are untouched, and
	// the gateway still requires mTLS, so widening the names a client may VERIFY widens no authority.
	//
	// ‼️ IP SANs ONLY — NOT A SECOND DNS NAME, and the difference is a shipped security pin. The runner
	// verifies `len(leaf.DNSNames) != 1` in three places (enrollment.go, renewal.go, session.go): the
	// controller's certificate must name exactly ONE host. Adding `localhost` beside `control-plane`
	// broke every one of them with "controller certificate DNS identity is not exact", measured
	// 2026-08-06 on the real enrol path. The fix is not to loosen the pin to fit this change — an IP SAN
	// satisfies verification when a device dials an address, and DNSNames stays a set of one.
	serverTemplate := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: controllerDNS},
		DNSNames:     []string{controllerDNS},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("sign server certificate: %w", err)
	}

	if err := writeCertPEM(p.caCert, caDER); err != nil {
		return err
	}
	if err := writeKeyPEM(p.caKey, caKey); err != nil {
		return err
	}
	if err := writeCertPEM(p.serverCert, serverDER); err != nil {
		return err
	}
	return writeKeyPEM(p.serverKey, serverKey)
}

// serial returns a random 128-bit certificate serial.
func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		// crypto/rand failing is fatal for the whole init; a fixed serial keeps the
		// signature call total without inventing a fallible return here.
		return big.NewInt(1)
	}
	return n
}

// writeCertPEM writes a DER certificate as a 0644 PEM (public material).
func writeCertPEM(path string, der []byte) error {
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return os.WriteFile(path, block, 0o644)
}

// writeKeyPEM writes an EC private key as a 0600 PKCS#8 PEM — the form
// execution.NewFileCertIssuer and tls.LoadX509KeyPair both parse.
func writeKeyPEM(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal EC key: %w", err)
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	return os.WriteFile(path, block, 0o600)
}
