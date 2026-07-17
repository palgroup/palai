package execution

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"
)

// runnerCertTTL bounds an issued runner client certificate. It is short-lived — the
// runner enrolls and immediately opens its lease session — but comfortably outlives one
// enroll→connect window and small clock skew on a local host.
const runnerCertTTL = 5 * time.Minute

// FileCertIssuer implements CertIssuer with the local control-plane CA `palai init`
// writes into the .palai layout. It signs an enrolling runner's public key into a
// short-lived client certificate — the file-backed counterpart of the in-test CA the
// gateway conformance proof drives.
type FileCertIssuer struct {
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
}

// NewFileCertIssuer loads the PEM CA certificate and its EC private key (PKCS#8) from the
// .palai CA files.
func NewFileCertIssuer(certPath, keyPath string) (*FileCertIssuer, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read CA certificate: %w", err)
	}
	cert, err := parseCertPEM(certPEM)
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read CA key: %w", err)
	}
	key, err := parseECKeyPEM(keyPEM)
	if err != nil {
		return nil, err
	}
	return &FileCertIssuer{caCert: cert, caKey: key}, nil
}

// SignRunnerCertificate signs the runner's public key into a short-lived client
// certificate under runnerDNS, usable only for client authentication.
func (i *FileCertIssuer) SignRunnerCertificate(publicKeyDER []byte, runnerDNS string) ([]byte, error) {
	parsed, err := x509.ParsePKIXPublicKey(publicKeyDER)
	if err != nil {
		return nil, fmt.Errorf("parse runner public key: %w", err)
	}
	pub, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("runner public key is not an ECDSA key")
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: runnerDNS},
		DNSNames:     []string{runnerDNS},
		NotBefore:    now.Add(-time.Second),
		NotAfter:     now.Add(runnerCertTTL),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	return x509.CreateCertificate(rand.Reader, template, i.caCert, pub, i.caKey)
}

// FileEnrollmentTokens implements EnrollmentTokens against the one-line token file `palai
// local up` mints fresh on every boot. Consume re-reads the file each call so a re-up that
// rotated the token is honored, while an in-memory spent set enforces one-use: the same
// token is redeemed at most once per control-plane process, so a replayed token mints no
// second identity.
type FileEnrollmentTokens struct {
	path     string
	mu       sync.Mutex
	consumed map[string]bool
}

// NewFileEnrollmentTokens binds the token file path; the file is read at Consume time.
func NewFileEnrollmentTokens(path string) *FileEnrollmentTokens {
	return &FileEnrollmentTokens{path: path, consumed: map[string]bool{}}
}

// Consume spends the current file token exactly once. It returns an error for an empty,
// unknown (not matching the current file), or already-spent token.
func (t *FileEnrollmentTokens) Consume(token string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if strings.TrimSpace(token) == "" {
		return errors.New("empty enrollment token")
	}
	raw, err := os.ReadFile(t.path)
	if err != nil {
		return fmt.Errorf("read enrollment token: %w", err)
	}
	if strings.TrimSpace(string(raw)) != token {
		return errors.New("unknown enrollment token")
	}
	if t.consumed[token] {
		return errors.New("enrollment token already spent")
	}
	t.consumed[token] = true
	return nil
}

func parseCertPEM(data []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("CA certificate file held no PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	return cert, nil
}

func parseECKeyPEM(data []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("CA key file held no PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("CA key is not an ECDSA key")
	}
	return key, nil
}
