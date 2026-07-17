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
	"os"
	"time"
)

// writeLocalCA generates the local control-plane CA and the runner-gateway server
// certificate `init` writes into .palai/ca. The CA (PKCS#8 EC key) is what
// execution.NewFileCertIssuer loads to sign enrolling runners; the server certificate
// carries exactly one SAN — controllerDNS — because the runner session pins the
// controller's DNS identity exactly (packages/runner). Keys are 0600; the public certs
// are 0644.
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
	serverTemplate := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: controllerDNS},
		DNSNames:     []string{controllerDNS},
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
