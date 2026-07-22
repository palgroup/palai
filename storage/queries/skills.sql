-- Skills registry management + run-pin resolution (spec §28.15-28.16, TOL-011, E12 Task 7). Writes are
-- the admin management surface (create/install/enable) — there is NO model-facing install path. A skill
-- is UNTRUSTED content: install stores the QUARANTINE-sanitized archive + digest + findings + metadata;
-- the ONLY UPDATE is the state transition (approve/enable). Every statement is tenant-scoped by
-- (organization_id, project_id). Reads serve the run-start pin resolver and workspace materialization.

-- name: InsertSkill
INSERT INTO skills (id, organization_id, project_id, name)
VALUES ($1, $2, $3, $4);

-- SkillExists verifies a skill lineage is in scope before a revision is attached to it.
-- name: SkillExists
SELECT 1 FROM skills WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- InsertSkillRevision stores one installed revision. revision_number is the skill's next monotonic
-- number, computed in-statement. state is 'approved' when the scan is clean, else 'quarantined'
-- (findings block the promotion). digest content-addresses the sanitized archive. Returns the number.
-- ponytail: the MAX+1 subselect can race two concurrent installs; UNIQUE(skill_id, revision_number)
-- then rejects the loser (retry on 23505 if it ever matters).
-- name: InsertSkillRevision
INSERT INTO skill_revisions (id, organization_id, project_id, skill_id, revision_number,
        digest, state, scan_findings, metadata, archive, source_url)
VALUES ($1, $2, $3, $4,
        (SELECT COALESCE(MAX(revision_number), 0) + 1 FROM skill_revisions WHERE skill_id = $4),
        $5, $6, $7, $8, $9, $10)
RETURNING revision_number;

-- GetSkillRevision reads a revision's state + digest + findings + metadata (management GET + the enable
-- gate's findings check).
-- name: GetSkillRevision
SELECT skill_id, revision_number, digest, state, scan_findings, metadata, source_url, created_at
FROM skill_revisions
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- EnableSkillRevision is the enable transition: approved→enabled, a once-only conditional flip. The
-- WHERE state = 'approved' guard makes a findings-bearing (quarantined) revision unenablable and an
-- already-enabled one a zero-row no-op. RETURNING id distinguishes enabled-now from not-approved/unknown.
-- name: EnableSkillRevision
UPDATE skill_revisions
SET state = 'enabled'
WHERE id = $1 AND organization_id = $2 AND project_id = $3 AND state = 'approved'
RETURNING id;

-- ResolveEnabledSkill resolves a skill NAME to its ACTIVE enabled revision's digest + metadata (the
-- run-start pin resolver, spec §28.16). Highest enabled revision_number wins; a name with no enabled
-- revision returns no rows, which the resolver turns into a visible run-start failure (an unknown or
-- not-enabled skill never silently no-ops).
-- name: ResolveEnabledSkill
SELECT sr.digest, sr.metadata
FROM skill_revisions sr
JOIN skills s ON s.id = sr.skill_id
WHERE s.organization_id = $1 AND s.project_id = $2 AND s.name = $3 AND sr.state = 'enabled'
ORDER BY sr.revision_number DESC
LIMIT 1;

-- LoadSkillArchive loads the sanitized archive bytes by digest (workspace materialization). The digest
-- content-addresses the sanitized tar, so any tenant-scoped revision carrying it is byte-equivalent.
-- name: LoadSkillArchive
SELECT archive FROM skill_revisions
WHERE organization_id = $1 AND project_id = $2 AND digest = $3
LIMIT 1;

-- ListSkills lists a project's skill lineages (management GET).
-- name: ListSkills
SELECT id, name, created_at FROM skills
WHERE organization_id = $1 AND project_id = $2
ORDER BY created_at;
