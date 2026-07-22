package extensions

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"

	"github.com/palgroup/palai/packages/egress"
	"github.com/palgroup/palai/storage"
)

var (
	// ErrSkillNameCollision is returned when a skill name is already taken in the project (create is a
	// REJECT, never an auto-suffix — the tool-registry precedent).
	ErrSkillNameCollision = errors.New("extensions: skill name already exists in this project")
	// ErrSkillNotFound is returned when a revision targets a skill absent from the scope, or a lifecycle
	// action names a revision absent from the scope.
	ErrSkillNotFound = errors.New("extensions: skill not found in scope")
	// ErrScanFindingsBlockEnable is returned when enable is attempted on a revision that carries static-scan
	// findings — it is stuck at quarantined and can never be enabled (TOL-011, scan FAIL blocks enable).
	ErrScanFindingsBlockEnable = errors.New("extensions: skill revision has scan findings and cannot be enabled")
	// ErrSkillMetadataMissing is returned when an installed archive has no SKILL.md — a skill must carry the
	// convention file whose frontmatter becomes the model-visible metadata rider.
	ErrSkillMetadataMissing = errors.New("extensions: skill archive has no SKILL.md")
)

// Skill-install network caps (spec §28.15). The compressed archive is bounded independently of the
// uncompressed extract cap so a hostile server cannot stream an unbounded download.
const (
	maxSkillFetchBytes = maxSkillArchiveBytes // the compressed upload should never exceed the uncompressed cap
	skillFetchTimeout  = 30 * time.Second
	skillMaxRedirects  = 5
	skillFetchUA       = "palai-skills/1"
)

// SkillMetadata is the parsed SKILL.md frontmatter (Agent Skills convention, spec §28.16). Name +
// Description become the model-visible progressive-loading rider; RequiredTools is the skill's REQUEST
// for tools — it is NEVER an authority grant (TOL-011): the effective set stays the revision-ceiling
// intersection, so a requested tool the run does not already carry is never advertised or dispatchable.
type SkillMetadata struct {
	Name          string   `json:"name" yaml:"name"`
	Description   string   `json:"description" yaml:"description"`
	RequiredTools []string `json:"required_tools,omitempty" yaml:"required_tools"`
}

// Skill is a skill lineage projection.
type Skill struct {
	ID   string
	Name string
}

// SkillRevision is an installed revision projection: the sanitized-archive digest, the lifecycle state,
// the static-scan findings, and the parsed metadata. It carries NO archive bytes (those materialize into
// a run's workspace, never a management response).
type SkillRevision struct {
	ID             string
	SkillID        string
	RevisionNumber int
	Digest         string
	State          string
	Findings       []SkillFinding
	Metadata       SkillMetadata
}

// SkillPin is one entry of a run's frozen skill set (spec §28.16): the model-visible name + description
// (the progressive-loading rider), the exact digest the run resolved at start (never "latest"), and the
// workspace-relative path the body materializes at (read on-demand via the file tool). It grants NO
// capability — it is a context reference plus a file location, nothing more.
type SkillPin struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Digest      string `json:"digest"`
	Path        string `json:"path"`
}

// skillFetchOptions carries the test-only network seams (a production build passes none), mirroring the
// research fetch tool: an injected resolver, a low-level dialer the pinned dialer hands vetted IPs to,
// and a TLS config trusting a test server.
type skillFetchOptions struct {
	resolver  egress.Resolver
	dial      func(ctx context.Context, network, addr string) (net.Conn, error)
	tlsConfig *tls.Config
}

// SkillFetchOption configures the install-by-URL fetch (test seams only).
type SkillFetchOption func(*skillFetchOptions)

// WithSkillResolver injects the DNS resolver the pinned dialer re-resolves through.
func WithSkillResolver(r egress.Resolver) SkillFetchOption {
	return func(o *skillFetchOptions) { o.resolver = r }
}

// WithSkillDialContext injects the low-level dialer; the pinned dialer always hands it a vetted IP.
func WithSkillDialContext(d func(ctx context.Context, network, addr string) (net.Conn, error)) SkillFetchOption {
	return func(o *skillFetchOptions) { o.dial = d }
}

// WithSkillTLSConfig injects a TLS config (a test trusts an httptest server's cert; production leaves it
// nil for the system roots — TLS is always required).
func WithSkillTLSConfig(c *tls.Config) SkillFetchOption {
	return func(o *skillFetchOptions) { o.tlsConfig = c }
}

// CreateSkill registers a named skill lineage. A name collision is a REJECT (ErrSkillNameCollision).
func (s *Store) CreateSkill(ctx context.Context, org, project, name string) (Skill, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Skill{}, fmt.Errorf("%w: empty name", ErrSkillNotFound)
	}
	id := newID("skill")
	if _, err := s.pool.Exec(ctx, storage.Query("InsertSkill"), id, org, project, name); err != nil {
		if isUniqueViolation(err) {
			return Skill{}, ErrSkillNameCollision
		}
		return Skill{}, fmt.Errorf("insert skill: %w", err)
	}
	return Skill{ID: id, Name: name}, nil
}

// InstallSkillRevision QUARANTINES a raw archive, parses its SKILL.md metadata, and stores an immutable
// revision (spec §28.15-28.16). A clean scan lands in 'approved' (cleared to enable); any finding keeps
// it 'quarantined' (unenablable). It stores the SANITIZED archive + the digest over it, never the raw
// upload. An unsafe archive (traversal/symlink/special/bomb) is rejected here, before any row is written.
func (s *Store) InstallSkillRevision(ctx context.Context, org, project, skillID string, archive []byte, sourceURL string) (SkillRevision, error) {
	switch err := s.pool.QueryRow(ctx, storage.Query("SkillExists"), skillID, org, project).Scan(new(int)); {
	case errors.Is(err, pgx.ErrNoRows):
		return SkillRevision{}, ErrSkillNotFound
	case err != nil:
		return SkillRevision{}, fmt.Errorf("check skill: %w", err)
	}

	q, err := Quarantine(archive)
	if err != nil {
		return SkillRevision{}, err // ErrUnsafeArchive — an unsafe upload never becomes a revision
	}
	meta, err := parseSkillMetadata(q.Sanitized)
	if err != nil {
		return SkillRevision{}, err
	}

	state := "approved"
	if len(q.Findings) > 0 {
		state = "quarantined" // findings block promotion; the revision can never be enabled until re-installed clean
	}
	findingsJSON, _ := json.Marshal(q.Findings)
	metaJSON, _ := json.Marshal(meta)

	id := newID("skillrev")
	var revNumber int
	if err := s.pool.QueryRow(ctx, storage.Query("InsertSkillRevision"),
		id, org, project, skillID, q.Digest, state, findingsJSON, metaJSON, q.Sanitized, nullableSource(sourceURL),
	).Scan(&revNumber); err != nil {
		return SkillRevision{}, fmt.Errorf("insert skill revision: %w", err)
	}
	return SkillRevision{ID: id, SkillID: skillID, RevisionNumber: revNumber, Digest: q.Digest, State: state, Findings: q.Findings, Metadata: meta}, nil
}

// InstallSkillRevisionFromURL fetches an archive over the hardened egress path (VetURL + PinnedDialer,
// every redirect re-vetted, NO credential of any kind on the wire) and installs it (spec §28.15). It is
// an ADMIN action — there is no model-facing install surface. allowPrivate is hard-false: a skill source
// is a fully-untrusted SSRF primitive, so a private/loopback/metadata target is never reachable.
func (s *Store) InstallSkillRevisionFromURL(ctx context.Context, org, project, skillID, rawURL string, opts ...SkillFetchOption) (SkillRevision, error) {
	archive, err := fetchSkillArchive(ctx, rawURL, opts...)
	if err != nil {
		return SkillRevision{}, err
	}
	return s.InstallSkillRevision(ctx, org, project, skillID, archive, rawURL)
}

// EnableSkillRevision transitions an approved revision to enabled. A revision with scan findings is
// stuck at quarantined and returns ErrScanFindingsBlockEnable; an unknown revision returns exists=false;
// an already-enabled revision is an idempotent success.
func (s *Store) EnableSkillRevision(ctx context.Context, org, project, revisionID string) (exists bool, err error) {
	rev, found, err := s.GetSkillRevision(ctx, org, project, revisionID)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	if rev.State == "enabled" {
		return true, nil // idempotent
	}
	if len(rev.Findings) > 0 || rev.State == "quarantined" {
		return true, ErrScanFindingsBlockEnable
	}
	var id string
	switch err := s.pool.QueryRow(ctx, storage.Query("EnableSkillRevision"), revisionID, org, project).Scan(&id); {
	case errors.Is(err, pgx.ErrNoRows):
		return true, ErrScanFindingsBlockEnable // not in 'approved' (raced/quarantined) — never silently enabled
	case err != nil:
		return true, fmt.Errorf("enable skill revision: %w", err)
	}
	return true, nil
}

// GetSkillRevision reads a revision's state/digest/findings/metadata.
func (s *Store) GetSkillRevision(ctx context.Context, org, project, revisionID string) (SkillRevision, bool, error) {
	var (
		skillID, digest, state string
		revNumber              int
		findingsJSON, metaJSON []byte
		sourceURL              *string
		createdAt              time.Time
	)
	err := s.pool.QueryRow(ctx, storage.Query("GetSkillRevision"), revisionID, org, project).
		Scan(&skillID, &revNumber, &digest, &state, &findingsJSON, &metaJSON, &sourceURL, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SkillRevision{}, false, nil
	}
	if err != nil {
		return SkillRevision{}, false, fmt.Errorf("read skill revision: %w", err)
	}
	rev := SkillRevision{ID: revisionID, SkillID: skillID, RevisionNumber: revNumber, Digest: digest, State: state}
	_ = json.Unmarshal(findingsJSON, &rev.Findings)
	_ = json.Unmarshal(metaJSON, &rev.Metadata)
	return rev, true, nil
}

// ResolveEnabledSkills resolves each requested skill NAME to its active enabled revision's pin
// (spec §28.16): the frozen {name, description, digest, path} a run records at start. An unknown or
// not-enabled name is a VISIBLE error — the run fails at start rather than silently dropping a skill the
// agent revision requested. path is the workspace-relative body location the file tool reads on-demand.
func (s *Store) ResolveEnabledSkills(ctx context.Context, org, project string, names []string) ([]SkillPin, error) {
	pins := make([]SkillPin, 0, len(names))
	for _, name := range names {
		var digest string
		var metaJSON []byte
		err := s.pool.QueryRow(ctx, storage.Query("ResolveEnabledSkill"), org, project, name).Scan(&digest, &metaJSON)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: %q is not an enabled skill", ErrSkillNotFound, name)
		}
		if err != nil {
			return nil, fmt.Errorf("resolve skill %q: %w", name, err)
		}
		var meta SkillMetadata
		_ = json.Unmarshal(metaJSON, &meta)
		pins = append(pins, SkillPin{Name: name, Description: meta.Description, Digest: digest, Path: SkillBodyPath(name)})
	}
	return pins, nil
}

// ListSkills lists a project's skill lineages (management GET).
func (s *Store) ListSkills(ctx context.Context, org, project string) ([]Skill, error) {
	rows, err := s.pool.Query(ctx, storage.Query("ListSkills"), org, project)
	if err != nil {
		return nil, fmt.Errorf("list skills: %w", err)
	}
	defer rows.Close()
	var out []Skill
	for rows.Next() {
		var sk Skill
		var createdAt time.Time
		if err := rows.Scan(&sk.ID, &sk.Name, &createdAt); err != nil {
			return nil, fmt.Errorf("scan skill: %w", err)
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

// LoadSkillArchive loads the sanitized archive bytes for a digest (workspace materialization).
func (s *Store) LoadSkillArchive(ctx context.Context, org, project, digest string) ([]byte, error) {
	var archive []byte
	err := s.pool.QueryRow(ctx, storage.Query("LoadSkillArchive"), org, project, digest).Scan(&archive)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: no archive for digest %s", ErrSkillNotFound, digest)
	}
	if err != nil {
		return nil, fmt.Errorf("load skill archive: %w", err)
	}
	return archive, nil
}

// SkillBodyPath is the workspace-relative directory a skill's body materializes under — a sibling of the
// repo, never inside it, so it never enters a changeset diff. The SKILL.md rides at <dir>/SKILL.md.
func SkillBodyPath(name string) string {
	return ".palai/skills/" + name + "/SKILL.md"
}

// parseSkillMetadata extracts SKILL.md from the sanitized tar and parses its YAML frontmatter (Agent
// Skills convention, spec §28.16). A missing SKILL.md is a hard error; a present one with no frontmatter
// yields empty metadata (valid — the body is still readable, just unnamed). required_tools is captured
// as the skill's REQUEST only — this function never grants anything.
func parseSkillMetadata(sanitizedTar []byte) (SkillMetadata, error) {
	tr := tar.NewReader(bytes.NewReader(sanitizedTar))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return SkillMetadata{}, fmt.Errorf("read sanitized archive: %w", err)
		}
		if strings.EqualFold(hdr.Name, "SKILL.md") {
			body, err := io.ReadAll(io.LimitReader(tr, maxSkillFileBytes+1))
			if err != nil {
				return SkillMetadata{}, fmt.Errorf("read SKILL.md: %w", err)
			}
			return parseFrontmatter(body), nil
		}
	}
	return SkillMetadata{}, ErrSkillMetadataMissing
}

// parseFrontmatter reads a leading `---\n ... \n---` YAML block into SkillMetadata. Malformed or absent
// frontmatter yields empty metadata rather than an error — the untrusted body must not be able to fail
// the install by shaping its own header; the security gate is the quarantine scan, not YAML validity.
func parseFrontmatter(body []byte) SkillMetadata {
	s := string(body)
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		// No frontmatter block — some skills carry only flat `name:`/`description:` lines; try the whole
		// head as YAML best-effort, ignoring parse errors.
		var meta SkillMetadata
		_ = yaml.Unmarshal(body, &meta)
		return meta
	}
	rest := s[strings.IndexByte(s, '\n')+1:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return SkillMetadata{}
	}
	var meta SkillMetadata
	_ = yaml.Unmarshal([]byte(rest[:end]), &meta)
	return meta
}

// fetchSkillArchive downloads an archive over the hardened egress path, mirroring the research fetch tool
// (spec §28.15, EXT-004 idiom): https-only, GET-only, NO credential on the wire, the destination
// re-resolved and vetted at connect time and re-vetted on every redirect hop so a private/loopback/
// metadata target is never reachable. The body is capped so a hostile server cannot stream unbounded.
func fetchSkillArchive(ctx context.Context, rawURL string, opts ...SkillFetchOption) ([]byte, error) {
	o := &skillFetchOptions{}
	for _, opt := range opts {
		opt(o)
	}
	if err := egress.VetURL(rawURL, false); err != nil {
		return nil, fmt.Errorf("skill install: %w", err)
	}
	resolver := o.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	dial := o.dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: skillFetchTimeout}).DialContext
	}
	client := &http.Client{
		Timeout: skillFetchTimeout,
		Transport: &http.Transport{
			DisableKeepAlives:   true, // force a fresh dial (and re-resolve+re-vet) for the initial request and every redirect
			TLSClientConfig:     o.tlsConfig,
			TLSHandshakeTimeout: skillFetchTimeout,
			DialContext:         egress.PinnedDialer(resolver, false, dial),
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= skillMaxRedirects {
				return fmt.Errorf("skill install: too many redirects (> %d)", skillMaxRedirects)
			}
			return egress.VetURL(req.URL.String(), false) // re-vet each hop before following
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("skill install: build request: %w", err)
	}
	// The ONLY headers: an honest UA. No cookie, no Authorization, no platform credential ever.
	req.Header.Set("User-Agent", skillFetchUA)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("skill install: fetch: %w", unwrapSkillURLError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("skill install: fetch returned status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSkillFetchBytes+1))
	if err != nil {
		return nil, fmt.Errorf("skill install: read body: %w", err)
	}
	if len(raw) > maxSkillFetchBytes {
		return nil, fmt.Errorf("%w: downloaded archive exceeds %d bytes", ErrUnsafeArchive, maxSkillFetchBytes)
	}
	return raw, nil
}

func unwrapSkillURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func nullableSource(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
