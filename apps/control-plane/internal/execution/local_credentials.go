package execution

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/subtle"
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

// runnerCertTTL bounds an issued runner client certificate when no TTL is configured. It is
// short-lived — the runner enrolls and immediately opens its lease session — but comfortably
// outlives one enroll→connect window and small clock skew on a local host. A long-lived
// runner rolls its certificate forward before expiry over the cert-authenticated renew
// endpoint (never the bootstrap credential, which is not on that path at all).
const runnerCertTTL = 5 * time.Minute

// FileCertIssuer implements CertIssuer with the local control-plane CA `palai init`
// writes into the .palai layout. It signs an enrolling (or renewing) runner's public key
// into a short-lived client certificate — the file-backed counterpart of the in-test CA the
// gateway conformance proof drives.
type FileCertIssuer struct {
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	ttl    time.Duration
}

// NewFileCertIssuer loads the PEM CA certificate and its EC private key (PKCS#8) from the
// .palai CA files. A non-positive ttl uses runnerCertTTL; the fault-live renewal proof
// injects a short TTL so a long-lived runner's rollover is provable in seconds.
func NewFileCertIssuer(certPath, keyPath string, ttl time.Duration) (*FileCertIssuer, error) {
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
	if ttl <= 0 {
		ttl = runnerCertTTL
	}
	return &FileCertIssuer{caCert: cert, caKey: key, ttl: ttl}, nil
}

// TTL is the effective issued-certificate lifetime (the configured value, or runnerCertTTL when
// none was configured) — the interval FileEnrollmentTokens rate-limits a token's redemption by.
func (i *FileCertIssuer) TTL() time.Duration { return i.ttl }

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
		NotAfter:     now.Add(i.ttl),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	return x509.CreateCertificate(rand.Reader, template, i.caCert, pub, i.caKey)
}

// FileEnrollmentTokens implements EnrollmentTokens against the one-line token file `palai
// local up` mints fresh on every boot. Consume re-reads the file each call so a re-up that
// rotated the token is honored.
//
// WHY THIS IS NOT ONE-USE, AND WHAT REPLACED THAT. Renewal runs over mTLS with the certificate
// that is expiring, so it serves only a runner still inside its validity window. A runner that
// missed the window — a sleeping laptop, a stalled Docker Desktop, a loaded host — holds a dead
// identity that can neither renew nor connect, and a strictly one-use token left it no way back:
// the stack bricked itself until an operator ran `palai local down && palai local up`, which
// nothing told them. So the file token is a BOOTSTRAP CREDENTIAL the runner may re-present when
// it holds no valid identity, not a one-shot.
//
// THREAT MODEL. The token grants exactly one capability: mint one runner client certificate.
// It lives in .palai/runner-token on the host, mounted read-only into the control-plane and the
// runner. Anyone who can read it therefore reads either (a) the host's .palai directory, which
// also holds ca/ca.key — the CA private key that signs runner certificates outright, a strictly
// greater capability than the token's — or (b) the runner container's filesystem, which mounts
// /var/run/docker.sock and is therefore already host root. Against both adversaries a replayable
// token grants nothing that was not already theirs, which is the bar: an attacker who cannot read
// the runner's own filesystem gains nothing. What one-use WAS load-bearing against is a token
// that leaks WITHOUT that access — copied into a shell history, a support bundle, a `docker
// inspect` — and minInterval is what stands in its place: at most one certificate is minted per
// token per issued-certificate lifetime.
//
// Be honest about what minInterval is: a RATE LIMIT, not an exclusion. A healthy runner rolls its
// identity forward over /v1/runner/renew, which never touches the token, so the interval elapses
// while that runner is alive and a token holder could mint a second, concurrent identity. It
// bounds replay VOLUME (no fleet from one leaked token) and it makes a runner restart wait out one
// certificate lifetime rather than re-minting at will.
//
// AND IT LIVES IN MEMORY, which E24 T3 measured rather than assumed (§3.6 D6): lastIssued is process
// state, so a control-plane restart resets the counter, and `restart: always` plus a host reboot does
// that on a schedule. The registry this comment used to name as the missing upgrade path now EXISTS
// (E24 T1, internal/fleet) and the durable counterpart of this timestamp is runner_pool_keys.last_used_at
// — but it is deliberately NOT a rate limit there, because a pool key is a fleet credential presented by
// many machines and one-redemption-per-lifetime would make a ten-machine pool unenrollable. So the
// honest statement is: nothing anywhere caps the number of concurrent identities one credential can
// mint. See storage/queries/runners.sql (TouchRunnerPoolKey) and internal/fleet/keys.go.
type FileEnrollmentTokens struct {
	path        string
	minInterval time.Duration
	mu          sync.Mutex
	lastIssued  map[string]time.Time
	now         func() time.Time
}

// NewFileEnrollmentTokens binds the token file path; the file is read at Consume time.
// minInterval is the shortest gap between two redemptions of the same token — pass the issued
// certificate TTL, so a token mints at most one certificate per certificate lifetime. Zero or
// negative disables the rate limit (every redemption is admitted).
func NewFileEnrollmentTokens(path string, minInterval time.Duration) *FileEnrollmentTokens {
	return &FileEnrollmentTokens{
		path:        path,
		minInterval: minInterval,
		lastIssued:  map[string]time.Time{},
		now:         time.Now,
	}
}

// Path is the bound token file, so a caller can name it in an operator-facing message.
func (t *FileEnrollmentTokens) Path() string { return t.path }

// Consume redeems the current file token. It returns an error for an empty token, one that does
// not match the current file, or one redeemed less than minInterval ago.
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
	// CONSTANT TIME, because this is a bearer credential and the comparison used to be `!=` (§3.6 D6).
	// The file token is presented by an unauthenticated caller over the network — that is the whole of
	// the enrolment endpoint — so the comparison is the one the rest of this tree already makes for a
	// presented secret (api/tool_callbacks.go:98, api/pagination.go:115). ConstantTimeCompare returns 0
	// for unequal lengths, which is the standard idiom and is what makes the empty-token guard above the
	// only length-dependent branch on this path.
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(string(raw))), []byte(token)) != 1 {
		return errors.New("unknown enrollment token")
	}
	now := t.now()
	if last, seen := t.lastIssued[token]; seen && now.Sub(last) < t.minInterval {
		return fmt.Errorf("enrollment token redeemed %s ago; it mints at most one certificate per %s",
			now.Sub(last).Round(time.Second), t.minInterval)
	}
	t.lastIssued[token] = now
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
