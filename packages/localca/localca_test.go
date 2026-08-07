package localca

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// paths lays a trust root inside a temp dir, in the same four-file shape compose bind-mounts.
func paths(t *testing.T) Paths {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ca")
	return Paths{
		CACert:     filepath.Join(dir, "ca.crt"),
		CAKey:      filepath.Join(dir, "ca.key"),
		ServerCert: filepath.Join(dir, "server.crt"),
		ServerKey:  filepath.Join(dir, "server.key"),
	}
}

// TestAnEmptyDeploymentGetsATrustRootItCanServe — THE CASE THAT MAKES `docker compose up -d` STAND ALONE.
//
// Before this package the four files existed only if `palai init` had run, so a control plane on a clean
// machine died in "load runner server certificate" and the cure was a CLI the deployment does not
// otherwise need. The assertions are the ones the gateway itself makes two statements after the call:
// the pair must load as a TLS keypair, and the CA must parse into a pool — a file that exists but does
// not satisfy those is the same outage with a longer path to it.
func TestAnEmptyDeploymentGetsATrustRootItCanServe(t *testing.T) {
	p := paths(t)

	minted, err := Ensure(p)
	if err != nil {
		t.Fatalf("Ensure on an empty deployment: %v", err)
	}
	if !minted {
		t.Fatal("Ensure reported it minted nothing on a deployment that had nothing — the caller cannot tell it happened")
	}

	if _, err := tls.LoadX509KeyPair(p.ServerCert, p.ServerKey); err != nil {
		t.Fatalf("the gateway cannot serve what was minted: %v", err)
	}
	caPEM, err := os.ReadFile(p.CACert)
	if err != nil {
		t.Fatalf("read the minted CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("the minted CA file held no certificates")
	}

	// ONE SAN, AND IT IS THE NAME THE RUNNER PINS. packages/runner refuses a controller certificate that
	// vouches for anyone else, so a second SAN here is a second party this plane can speak for.
	block, _ := pem.Decode(mustRead(t, p.ServerCert))
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse the server certificate: %v", err)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != ControllerDNS {
		t.Fatalf("server certificate SANs = %v, want exactly [%s]", leaf.DNSNames, ControllerDNS)
	}
	if len(leaf.IPAddresses) != 0 {
		t.Fatalf("server certificate carries IP SANs %v — a device reaches this plane by address and verifies it by NAME", leaf.IPAddresses)
	}

	// The CA must actually have signed the leaf, or the runner verifies against a root that vouches for
	// nothing it will be shown.
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: ControllerDNS}); err != nil {
		t.Fatalf("the server certificate does not verify against the CA minted beside it: %v", err)
	}

	// The keys are private material and the certificates are not.
	assertMode(t, p.CAKey, 0o600)
	assertMode(t, p.ServerKey, 0o600)
	assertMode(t, p.CACert, 0o644)
	assertMode(t, p.ServerCert, 0o644)
}

// TestACompleteTrustRootIsNeverReMinted — RE-MINTING IS A FLEET-WIDE OUTAGE ON A ROUTINE RESTART.
//
// Every runner already enrolled holds a certificate this CA signed. A boot that mints a second CA makes
// all of them untrusted at once, and it would do it silently, on the most ordinary event there is. The
// bytes are compared rather than the timestamps: a re-mint that happened to produce the same modes and
// the same shape is still a different root.
func TestACompleteTrustRootIsNeverReMinted(t *testing.T) {
	p := paths(t)
	if _, err := Ensure(p); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	before := map[string][]byte{}
	for _, f := range []string{p.CACert, p.CAKey, p.ServerCert, p.ServerKey} {
		before[f] = mustRead(t, f)
	}

	minted, err := Ensure(p)
	if err != nil {
		t.Fatalf("second Ensure on a complete root: %v", err)
	}
	if minted {
		t.Fatal("Ensure re-minted a complete trust root — every runner this CA had enrolled is now untrusted")
	}
	for f, want := range before {
		if got := mustRead(t, f); string(got) != string(want) {
			t.Fatalf("%s changed on a second Ensure", f)
		}
	}
}

// TestAPartialTrustRootIsRefusedByName — THE FAILURE THAT IS LOUDEST TO CAUSE AND QUIETEST TO CAUSE BY
// ACCIDENT.
//
// A CA certificate whose key is gone cannot sign the next enrolment; a server certificate whose CA is
// gone verifies for nobody. Both are recoverable by a human who knows which file was lost and NEITHER is
// recoverable by a program that guesses, so minting over the remainder would orphan the whole fleet on a
// boot that reported success. Each of the four files is removed in turn, because a refusal that only
// fires for one of them is a refusal three deletions can walk around.
func TestAPartialTrustRootIsRefusedByName(t *testing.T) {
	for _, missing := range []string{"ca.crt", "ca.key", "server.crt", "server.key"} {
		t.Run(missing, func(t *testing.T) {
			p := paths(t)
			if _, err := Ensure(p); err != nil {
				t.Fatalf("seed a complete root: %v", err)
			}
			gone := filepath.Join(filepath.Dir(p.CACert), missing)
			if err := os.Remove(gone); err != nil {
				t.Fatalf("remove %s: %v", gone, err)
			}

			minted, err := Ensure(p)
			if minted {
				t.Fatal("Ensure minted over a partial root")
			}
			if !errors.Is(err, ErrPartial) {
				t.Fatalf("Ensure(partial) error = %v, want ErrPartial", err)
			}
			// The operator has to know WHICH file to restore, and the message is the only place that
			// can tell them.
			if !strings.Contains(err.Error(), gone) {
				t.Fatalf("the refusal does not name the missing file %s: %v", gone, err)
			}
			// And it must not have written the survivors away while refusing.
			for _, f := range []string{p.CACert, p.CAKey, p.ServerCert, p.ServerKey} {
				if f == gone {
					continue
				}
				if _, statErr := os.Stat(f); statErr != nil {
					t.Fatalf("%s disappeared during a refusal: %v", f, statErr)
				}
			}
		})
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return raw
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %04o, want %04o", path, got, want)
	}
}
