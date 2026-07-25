package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// CheckpointVersion is the on-disk envelope version. Bumping it invalidates old checkpoints loudly
// (Compare raises AlertMalformed) rather than comparing across two different meanings of "head".
const CheckpointVersion = 1

// Checkpoint is the signed anchor kept OUTSIDE the database. Sessions are sorted by session_id so the
// file's bytes — and therefore its signature — depend only on the journal, not on row arrival order.
type Checkpoint struct {
	Version     int             `json:"version"`
	Algorithm   string          `json:"algorithm"`
	GeneratedAt string          `json:"generated_at"`
	Head        string          `json:"head"`
	Count       int             `json:"count"`
	Sessions    []SessionAnchor `json:"sessions"`
}

// Checkpoint file names. The envelope is the E14 T5 shape: the signed file, its sha256 manifest, a
// detached openssl signature, and the public key written BESIDE them for convenience — the verifier
// refuses to trust that copy (see ResolvePubkey).
const (
	CheckpointFile = "audit-checkpoint.json"
	ManifestFile   = CheckpointFile + ".sha256"
	SignatureFile  = CheckpointFile + ".sig"
	PubkeyFile     = "palai-audit-checkpoint.pub"
)

// AllowBundledPubkeyEnv opts in to trusting a public key that sits beside the checkpoint. It exists
// only for a same-session local proof; see ResolvePubkey.
const AllowBundledPubkeyEnv = "PALAI_AUDIT_ALLOW_BUNDLED_PUBKEY"

// NewCheckpoint summarizes rows into a checkpoint. now is injected so a test can pin the timestamp.
func NewCheckpoint(rows []Row, now time.Time) Checkpoint {
	head, sessions, count := Summarize(rows)
	if sessions == nil {
		sessions = []SessionAnchor{}
	}
	return Checkpoint{
		Version:     CheckpointVersion,
		Algorithm:   Algorithm,
		GeneratedAt: now.UTC().Format(time.RFC3339Nano),
		Head:        head,
		Count:       count,
		Sessions:    sessions,
	}
}

// WriteSigned writes the checkpoint envelope into dir and signs it with signingKey.
//
// SIGNING TOOL: `openssl dgst -sha256 -sign` over an ECDSA P-256 key — the E14 T5 signer's command
// set VERBATIM (scripts/package/runner/build.sh). This adds no second signing tool and no second
// keyring: it is the same binary, the same command, and the same key format the release signer uses,
// and scripts/release/runner-verify.sh verifies these bytes unchanged.
//
// A ZERO-ROW CHECKPOINT IS REFUSED unless allowEmpty says otherwise, and the refusal lives HERE, at
// the write boundary every caller routes through, rather than in the one command that happens to have
// noticed. A checkpoint cut from a read that returned nothing anchors the empty prefix: it verifies
// green against ANY journal forever, because every row is merely "unanchored". Nothing about the read
// has to be malicious to produce it — every tenant table is FORCE ROW LEVEL SECURITY (000029), so a
// connection without palai.org_id returns zero rows AND NO ERROR. That is the E14 T7 vacuous-scan
// lesson: a claim over a surface that can be empty can never fail. ReadRows refuses the scoped
// connection; this refuses the empty result whatever produced it.
func WriteSigned(dir string, cp Checkpoint, signingKey string, allowEmpty bool) error {
	if signingKey == "" {
		return errors.New("audit: a signing key is required to cut a checkpoint (an unsigned anchor is not an anchor)")
	}
	if cp.Count == 0 && !allowEmpty {
		return errors.New("audit: REFUSING to sign a checkpoint over ZERO rows — it would anchor the empty prefix and " +
			"verify green against any journal forever. Either the journal really is empty (pass --allow-empty to say so " +
			"out loud) or the connection could not see it (a non-superuser read under FORCE ROW LEVEL SECURITY returns " +
			"no rows and no error)")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	path := filepath.Join(dir, CheckpointFile)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	manifest := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), CheckpointFile)
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(manifest), 0o644); err != nil {
		return err
	}
	if out, err := exec.Command("openssl", "dgst", "-sha256", "-sign", signingKey,
		"-out", filepath.Join(dir, SignatureFile), path).CombinedOutput(); err != nil {
		return fmt.Errorf("audit: openssl sign failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("openssl", "pkey", "-in", signingKey, "-pubout",
		"-out", filepath.Join(dir, PubkeyFile)).CombinedOutput(); err != nil {
		return fmt.Errorf("audit: openssl pubout failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ResolvePubkey applies the fail-closed trust posture E18 T3 hardened
// (scripts/release/provenance-verify.sh): the public key must arrive OUT OF BAND. A key that resolves
// INSIDE the checkpoint's own directory is refused, because whoever rewrote the journal and re-cut the
// checkpoint could re-cut the key beside it too — then the signature is just a second checksum they
// control end to end. PALAI_AUDIT_ALLOW_BUNDLED_PUBKEY=1 says out loud that this is a same-session
// local proof.
func ResolvePubkey(checkpointPath, pubkey string) (string, string, error) {
	if pubkey == "" {
		return "", "", errors.New("audit: a trusted public key is REQUIRED (--pubkey or PALAI_AUDIT_PUBKEY); obtain it out of band, never from beside the checkpoint")
	}
	abs := realPath(pubkey)
	dir := realPath(filepath.Dir(checkpointPath))
	// Containment is decided on RESOLVED paths and by INODE, not by string prefix, because both string
	// forms are trivially defeated: a symlink outside the dir pointing at the bundled key reads as
	// "elsewhere", and reaching the checkpoint through a symlinked directory makes the real key path
	// read as "elsewhere" too. Both were accepted before, and the report then printed "key supplied
	// out of band" — asserting the exact property it did not have. os.SameFile also settles macOS
	// APFS case-insensitivity (/tmp/CP vs /tmp/cp), which no string compare gets right.
	//
	// Containment, not equality: a key tucked in a SUBDIRECTORY of the checkpoint dir is just as much
	// under the attacker's control (the provenance-verify.sh `"$rel_abs"/*` rule).
	if under(abs, dir) {
		if os.Getenv(AllowBundledPubkeyEnv) != "1" {
			return "", "", fmt.Errorf("audit: REFUSING — the public key %s resolves under the checkpoint directory %s it would verify, so it is not a trust anchor; pass an out-of-band key or set %s=1 for a same-session local proof", abs, dir, AllowBundledPubkeyEnv)
		}
		return abs, "WARNING: the public key came from beside the checkpoint (" + AllowBundledPubkeyEnv + "=1) — the signature proves only self-consistency; same-session local proof only", nil
	}
	return abs, "", nil
}

// realPath is the absolute, symlink-free form of p. A path that does not exist (a --pubkey typo) has
// no resolved form, and its absolute form is then the honest answer — openssl will produce the real
// error a moment later.
func realPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs
	}
	return resolved
}

// under reports whether path is dir or lives below it, walking up path's parents and comparing by
// inode. Both arguments must already be realPath'd.
func under(path, dir string) bool {
	target, err := os.Stat(dir)
	if err != nil {
		return false
	}
	for p := path; ; {
		if fi, err := os.Stat(p); err == nil && os.SameFile(fi, target) {
			return true
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false
		}
		p = parent
	}
}

// LoadSigned verifies the checkpoint envelope and returns the parsed checkpoint. Signature FIRST:
// nothing in the file is parsed as truth before the bytes are shown to be the signed ones. Any
// failure returns a typed AlertSignature so the caller reports it like every other integrity alert.
func LoadSigned(checkpointPath, pubkey string) (Checkpoint, string, *Alert) {
	resolved, warn, err := ResolvePubkey(checkpointPath, pubkey)
	if err != nil {
		return Checkpoint{}, "", &Alert{Kind: AlertSignature, Detail: err.Error()}
	}
	body, err := os.ReadFile(checkpointPath)
	if err != nil {
		return Checkpoint{}, "", &Alert{Kind: AlertSignature, Detail: "read checkpoint: " + err.Error()}
	}
	// The sha256 manifest is a corruption check, not a security control (an attacker rewrites both);
	// the signature below is the security control. Checking it first gives the honest error for a
	// truncated file rather than an opaque signature failure.
	manifest, err := os.ReadFile(checkpointPath + ".sha256")
	if err == nil {
		sum := sha256.Sum256(body)
		if !strings.HasPrefix(strings.TrimSpace(string(manifest)), hex.EncodeToString(sum[:])) {
			return Checkpoint{}, "", &Alert{Kind: AlertSignature, Detail: "the checkpoint bytes do not match their sha256 manifest"}
		}
	}
	sig := checkpointPath + ".sig"
	out, verr := exec.Command("openssl", "dgst", "-sha256", "-verify", resolved,
		"-signature", sig, checkpointPath).CombinedOutput()
	if verr != nil {
		return Checkpoint{}, warn, &Alert{
			Kind: AlertSignature,
			Detail: fmt.Sprintf("the checkpoint signature does not verify against %s: %s",
				resolved, strings.TrimSpace(string(out))),
		}
	}
	var cp Checkpoint
	if err := json.Unmarshal(body, &cp); err != nil {
		return Checkpoint{}, warn, &Alert{Kind: AlertMalformed, Detail: "parse checkpoint: " + err.Error()}
	}
	return cp, warn, nil
}
