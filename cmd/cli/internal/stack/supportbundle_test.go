package stack

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"
)

// The mandatory credential-hygiene test: a support bundle must carry ZERO secrets. It seeds the
// redactor with real .palai secret values AND feeds bundle parts that deliberately leak those
// values plus generic provider-key / bearer / OPENAI_API_KEY shapes, then reads the produced
// tar.gz back and asserts none of the needles survive — the redaction is proven, not asserted.
func TestSupportBundleRedactsAllSecrets(t *testing.T) {
	masterKey := "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	apiKey := "palai-0011223344556677889900aabbccddeeff00112233445566778899"
	pgPassword := "pg-supersecret-778899"
	providerKey := "sk-THISISASECRETPROVIDERKEY0000000000"

	red := newRedactor([]string{masterKey, apiKey, pgPassword, providerKey})

	// Parts that a real `compose logs` / `compose config` could plausibly leak — including the leak
	// SHAPES (quoted-spaces value, slash-leading base64, PEM, JSON, lowercase, db-URL cred) so the
	// whole writeBundle → tar pipeline (not just red.redact) is asserted to carry zero needles.
	parts := []bundlePart{
		{"doctor.json", []byte(`{"ok":true}`)},
		{"compose-logs.txt", []byte(
			"boot ok\n" +
				"master=" + masterKey + "\n" +
				"Authorization: Bearer sometokenvalue123abc\n" +
				"OPENAI_API_KEY=sk-anothersecretkey1111111111111111\n" +
				`GRAFANA_ADMIN_PASSWORD: "two words secret"` + "\n" +
				"RUNNER_TOKEN=/9j/base64slashleadingSECRET==\n" +
				`{"password":"jsonsecret999"}` + "\n" +
				"token=lowercasesecret777\n" +
				"pg dsn: postgres://palai:" + pgPassword + "@db/palai\n" +
				"-----BEGIN RSA PRIVATE KEY-----\npemsecretmaterial999\n-----END RSA PRIVATE KEY-----\n")},
		{"compose-config.yaml", []byte("bootstrap_api_key: " + apiKey + "\nprovider: " + providerKey + "\n")},
	}

	var buf bytes.Buffer
	if err := writeBundle(&buf, parts, red); err != nil {
		t.Fatalf("writeBundle: %v", err)
	}

	all := readTarGz(t, &buf)

	needles := []string{
		masterKey, apiKey, pgPassword, providerKey,
		"sk-anothersecretkey1111111111111111",
		"Bearer sometokenvalue123abc",
		"sometokenvalue123abc",
		"words secret", "base64slashleadingSECRET", "jsonsecret999", "lowercasesecret777",
		"pemsecretmaterial999",
	}
	for _, n := range needles {
		if strings.Contains(all, n) {
			t.Fatalf("secret leaked into the support bundle: %q", n)
		}
	}
	// The scrub actually ran (not simply an empty bundle).
	if !strings.Contains(all, "[REDACTED]") {
		t.Fatal("no [REDACTED] marker in the bundle — redaction did not run")
	}
	if !strings.Contains(all, "boot ok") {
		t.Fatal("non-secret log content was dropped — redaction is too aggressive")
	}
}

// readTarGz decompresses and concatenates every entry's bytes (plus its name) so a single
// substring scan covers the whole bundle.
func readTarGz(t *testing.T, r io.Reader) string {
	t.Helper()
	gz, err := gzip.NewReader(r)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	var out strings.Builder
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		out.WriteString(h.Name + "\n")
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", h.Name, err)
		}
		out.Write(b)
	}
	return out.String()
}

// Leak shapes the first redactor pass missed — each is a real secret surface a log or compose
// config can carry. The redactor must scrub every one; the assertions name the exact tail that
// leaked before the fix.
func TestRedactorLeakShapes(t *testing.T) {
	red := newRedactor(nil)
	cases := []struct {
		name   string
		in     string
		leaked string // the substring that must NOT survive
	}{
		// S-1: a quoted value with spaces — only the first word was redacted before.
		{"quoted-value-spaces", `GRAFANA_ADMIN_PASSWORD: "two words secret"`, "words secret"},
		// S-2: a base64 secret whose first char is `/` was kept as if it were a path.
		{"slash-leading-secret", `RUNNER_TOKEN=/9j/base64slashleading==`, "base64slashleading"},
		// S-3: a PEM private-key block.
		{"pem-block", "-----BEGIN RSA PRIVATE KEY-----\nMIIBOgetc/secretkeymaterial\n-----END RSA PRIVATE KEY-----", "secretkeymaterial"},
		// S-3: a JSON secret field.
		{"json-password", `{"host":"db","password":"jsonsecret123"}`, "jsonsecret123"},
		// S-3: a lowercase assignment.
		{"lowercase-token", `token=lowercasesecretvalue`, "lowercasesecretvalue"},
		// S-3: a database URL credential.
		{"pg-url-cred", `dsn: postgres://palai:urlpasswordsecret@db:5432/palai`, "urlpasswordsecret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(red.redact([]byte(tc.in)))
			if strings.Contains(got, tc.leaked) {
				t.Fatalf("secret leaked: %q survived in %q", tc.leaked, got)
			}
		})
	}
}

// A non-secret filesystem path in a *_KEY/_PASSWORD/_TOKEN assignment must stay (the stack's
// secrets are file-based — a `*_KEY: /path` names a path, and dropping it costs a diagnostic).
func TestRedactorKeepsPaths(t *testing.T) {
	red := newRedactor(nil)
	got := string(red.redact([]byte("PALAI_RUNNER_CA_KEY: /palai/ca/ca.key")))
	if !strings.Contains(got, "/palai/ca/ca.key") {
		t.Fatalf("a non-secret key PATH was over-redacted: %q", got)
	}
}

func TestRedactorGenericPatterns(t *testing.T) {
	// The redactor scrubs provider-key / bearer / env-secret shapes even with NO seeded literals,
	// so a secret the bundle assembler never knew about is still caught.
	red := newRedactor(nil)
	for _, s := range []string{
		"sk-abcdefghij0123456789ABCDEFG",
		"authorization: Bearer eyJhbGciOi.payload.sig",
		"PALAI_SECRET_MASTER_KEY=deadbeefdeadbeef",
	} {
		got := string(red.redact([]byte(s)))
		if !strings.Contains(got, "[REDACTED]") {
			t.Fatalf("pattern not redacted: %q -> %q", s, got)
		}
	}
}
