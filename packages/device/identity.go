package device

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"time"
)

// DeviceKey is the machine's durable identity: one keypair, generated on the first enrolment and never
// generated again.
//
// ‼️ THIS IS WHAT MAKES A RESTART A RESTART RATHER THAN A NEW MACHINE. Before it, `packages/runner.Enroll`
// generated a fresh key on every process start (enrollment.go, the `ecdsa.GenerateKey` at the top of
// Enroll), so a Mac that rebooted arrived as a machine the control plane had never seen, took a new
// `rnr_` id, and — in a strict pool — waited for a human to approve a box that was already approved.
// The registry now keys a machine on the fingerprint of this key (plan §3.4), so the id follows the key
// and the key follows the disk.
//
// THE HOSTNAME IS NOT IDENTITY AND NEITHER IS ANYTHING THE MACHINE TYPES. A client-supplied id is
// exactly the trust this design removes: possession of the private half is proved by signing the CSR,
// and nothing else the request carries decides who the machine is.
type DeviceKey struct {
	private *ecdsa.PrivateKey
}

// Fingerprint is the sha256 of the PUBLIC key's DER, hex-encoded — the same shape
// `runners.public_key_sha256` has always held, so a device and the registry compute one string the same
// way. A public key is not a credential and its digest is not a secret.
func (k DeviceKey) Fingerprint() string {
	der, err := x509.MarshalPKIXPublicKey(&k.private.PublicKey)
	if err != nil {
		// MarshalPKIXPublicKey cannot fail for a P-256 public key this package generated itself; an
		// empty fingerprint would be a value that silently matches nothing, so this is unreachable by
		// construction rather than handled.
		return ""
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// Signer exposes the private half for TLS. It is deliberately the only way out of this type: the key is
// never marshalled by a caller, so there is one writer of the key file and it is in this package.
func (k DeviceKey) Signer() *ecdsa.PrivateKey { return k.private }

// CertificateRequest builds the PKCS#10 the control plane verifies. The signature over the request body
// is the proof of possession the old enrolment had none of — its comment said so:
// "ponytail: no CSR proof-of-possession" — which meant anyone who could present a pool key could enrol
// ANY public key, including one whose private half they did not hold.
//
// The subject carries the machine's LABEL and nothing that decides anything. The certificate the server
// issues names the id the server minted, so a CommonName here is inventory, not a request.
func (k DeviceKey) CertificateRequest(label string) ([]byte, error) {
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:            pkix.Name{CommonName: label},
		SignatureAlgorithm: x509.ECDSAWithSHA256,
	}, k.private)
	if err != nil {
		return nil, fmt.Errorf("build certificate request: %w", err)
	}
	return der, nil
}

// LoadOrCreateDeviceKey returns the machine's durable key, generating and persisting one on first call.
//
// ‼️ THE EXISTING KEY IS NEVER OVERWRITTEN, and that is the invariant the whole recovery path rests on.
// A second `enroll` on an installed machine reuses the key and therefore recovers the SAME runner id;
// a function that minted a new one would turn "re-run the installer" into "become a different machine",
// which is precisely the behaviour being removed.
//
// The file's mode is checked before it is read, for the reason RequireOwnerOnly gives: a private key any
// account can read is not a device identity, it is a shared one.
func LoadOrCreateDeviceKey(path string) (DeviceKey, error) {
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := RequireOwnerOnly(path); err != nil {
			return DeviceKey{}, err
		}
		block, _ := pem.Decode(raw)
		if block == nil || block.Type != "PRIVATE KEY" {
			return DeviceKey{}, fmt.Errorf("%s does not contain a PEM private key", path)
		}
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return DeviceKey{}, fmt.Errorf("parse %s: %w", path, err)
		}
		key, ok := parsed.(*ecdsa.PrivateKey)
		if !ok {
			return DeviceKey{}, fmt.Errorf("%s holds a %T, not an ECDSA key", path, parsed)
		}
		return DeviceKey{private: key}, nil
	case !isNotExist(err):
		return DeviceKey{}, err
	}

	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return DeviceKey{}, fmt.Errorf("generate device key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return DeviceKey{}, fmt.Errorf("marshal device key: %w", err)
	}
	if err := ensureDir(pathDir(path)); err != nil {
		return DeviceKey{}, err
	}
	if err := WriteOwnerOnly(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil {
		return DeviceKey{}, err
	}
	return DeviceKey{private: private}, nil
}

// DeviceIdentity is what the control plane answered: the id it minted for this device key and the
// certificate it issued. It is rewritten on every renewal and the KEY beside it is not.
//
// ‼️ AN EXPIRED IDENTITY IS STILL LOAD-BEARING and that is why this file is kept rather than deleted when
// the certificate lapses. Renewal authenticates with the certificate that is expiring, so a machine that
// missed its window has nothing to renew with — what it has is this id and the device key, and presenting
// both plus the pool key is the recovery that reissues the SAME id (plan §3.4). Deleting a stale identity
// would turn a laptop that slept through its renewal into a new machine.
type DeviceIdentity struct {
	RunnerID string `json:"runner_id"`
	// Certificate is the issued leaf, base64 DER (encoding/json emits []byte as base64). Empty is a
	// legitimate state: an enrolment that recorded an id and failed before the certificate arrived.
	Certificate []byte `json:"certificate,omitempty"`
	// NotAfter is the leaf's expiry, recorded so a caller can tell "expired" from "absent" without
	// parsing the certificate. It is a convenience and never the authority — the certificate is.
	NotAfter time.Time `json:"not_after,omitempty"`
	// Fingerprint is the device key this identity belongs to. It is written so a machine can DETECT the
	// mismatch itself rather than presenting a stale id under a new key and being refused by the server:
	// re-imaging a disk that kept the identity file and lost the key is exactly that shape.
	Fingerprint string `json:"public_key_sha256"`
}

// SaveIdentity writes the identity atomically at 0600.
func SaveIdentity(path string, identity DeviceIdentity) error {
	encoded, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return fmt.Errorf("encode device identity: %w", err)
	}
	return WriteOwnerOnly(path, append(encoded, '\n'))
}

// LoadIdentity reads the persisted identity. A machine that has never enrolled has none, which is not an
// error — the zero value says "no id to recover" and the enrolment asks for a new one.
//
// AN IDENTITY WHOSE FINGERPRINT IS NOT THIS KEY'S IS DISCARDED HERE rather than sent. It would be
// refused by the control plane (a persisted id and a fingerprint that disagree is one of the four
// refusals plan §T2 names), and refusing it locally means a re-imaged machine enrols as the NEW machine
// it is instead of failing forever with a message about an identity it cannot prove.
func LoadIdentity(path string, key DeviceKey) (DeviceIdentity, error) {
	if err := RequireOwnerOnly(path); err != nil {
		if isNotExist(err) {
			return DeviceIdentity{}, nil
		}
		return DeviceIdentity{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if isNotExist(err) {
			return DeviceIdentity{}, nil
		}
		return DeviceIdentity{}, err
	}
	var identity DeviceIdentity
	if err := json.Unmarshal(raw, &identity); err != nil {
		return DeviceIdentity{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if identity.Fingerprint != "" && identity.Fingerprint != key.Fingerprint() {
		return DeviceIdentity{}, nil
	}
	return identity, nil
}

// pathDir is filepath.Dir, named locally so identity.go does not import path/filepath for one call and
// so the directory a key is written into is created with the same 0700 every other state file gets.
func pathDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == os.PathSeparator {
			return path[:i]
		}
	}
	return "."
}
