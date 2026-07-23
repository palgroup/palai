SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

.PHONY: \
	bootstrap generate check-generated lint test-unit test-component test-e2e \
	test-fault test-security test-live-provider test-live-hook-deny test-live-tenancy test-live-second-tenant test-live-run-history test-spikes evidence-spikes \
	check-spike-reports verify local-up local-down local-doctor uat-local-live \
	uat-interactive uat-coding uat-recovery uat-automation uat-extensibility uat-managed-cloud uat-self-host \
	uat-kubernetes uat-kind evidence-verify migration-resume-drill

bootstrap:
	go mod download
	uv sync --locked
	uv sync --locked --project engines/reference
	pnpm install --frozen-lockfile

generate:
	@test -x scripts/contracts/generate || { echo "contracts capability not implemented" >&2; exit 2; }
	@scripts/contracts/generate

check-generated:
	@test -x scripts/contracts/check || { echo "contracts capability not implemented" >&2; exit 2; }
	@scripts/contracts/check
# The TypeScript SDK rides this tier: it already warms node+pnpm (and diffs the SDK's
# generated types above), and nothing else in verify typechecks src/ or runs the suite.
	@pnpm --dir sdks/typescript run typecheck
	@pnpm --dir sdks/typescript test

lint:
	@git diff --check
	@find scripts -type f -name '*.sh' -print0 | xargs -0 bash -n
	@files="$$(git ls-files '*.go')"; \
	if test -n "$$files"; then \
		unformatted="$$(gofmt -l $$files)"; \
		test -z "$$unformatted" || { printf '%s\n' "$$unformatted" >&2; exit 1; }; \
	fi

test-unit:
	@bash scripts/test/foundation.sh
	@packages="$$(go list ./... 2>/dev/null || true)"; \
	if test -n "$$packages"; then go test ./...; fi
	@uv run --locked --project engines/reference pytest engines/reference/tests -q

test-spikes:
	@bash scripts/test/spikes.sh
	@scripts/spikes/run quick

evidence-spikes:
	@scripts/spikes/run evidence

check-spike-reports:
	@scripts/spikes/check-reports

test-component:
	@test -x scripts/test/component || { echo "component suite not implemented" >&2; exit 2; }
	@scripts/test/component

# E15 T1 live interruption/resume drill (OPS-006): kills a REAL control-plane binary mid migration chain
# and proves the restart resumes the journal to the head with data intact. Throwaway Postgres, no
# credentials. Not part of verify (Docker-bound).
migration-resume-drill:
	@bash scripts/test/migration-resume-drill.sh

test-e2e:
	@test -x scripts/test/e2e || { echo "end-to-end suite not implemented" >&2; exit 2; }
	@scripts/test/e2e

test-fault:
	@test -x scripts/test/fault || { echo "fault suite not implemented" >&2; exit 2; }
	@scripts/test/fault

test-security:
	@test -x scripts/test/security || { echo "security suite not implemented" >&2; exit 2; }
	@scripts/test/security

# Most protected tier: real provider over the network, credential loaded from the
# git-ignored .env.local at runtime. Not part of verify. Select the case with
# `make test-live-provider PROVIDER=provider-one CASE=text-stream-tool-schema`.
test-live-provider:
	@test -x scripts/test/live-provider || { echo "live provider suite not implemented" >&2; exit 2; }
	@scripts/test/live-provider

# E12 Task 8 approved live smoke: a real provider spontaneously calls a tool, a before_tool policy hook
# denies it, and the model sees the structured control-plane deny mid-run (spec §28.17). A convenience
# alias for the CASE=hook-deny-visible live-provider case (PROVIDER=provider-one).
test-live-hook-deny:
	@PROVIDER=provider-one CASE=hook-deny-visible scripts/test/live-provider

# E13 Task 1 live smoke: a two-org stack proves cross-tenant isolation at both layers — org-B's key gets
# a 404 on org-A's REAL provider run, and an org-B-scoped DB read is denied org-A's rows by RLS (000029).
# A convenience alias for the CASE=tenancy-isolation live-provider case (PROVIDER=provider-one).
test-live-tenancy:
	@PROVIDER=provider-one CASE=tenancy-isolation scripts/test/live-provider

# E13 Task 4 live smoke: over the REAL router a tenant lists its run history (a REAL provider-one run) with
# a plain HTTP client, and a second tenant presenting the first tenant's cursor is rejected with 400
# invalid_cursor while its own history is RLS-empty (MCI-003 + TEN-001 cursor-fuzz). A convenience alias for
# the CASE=run-history-list live-provider case (PROVIDER=provider-one).
test-live-run-history:
	@PROVIDER=provider-one CASE=run-history-list scripts/test/live-provider

# E13 Task 2 live smoke: a running store provisions a BRAND-NEW tenant via the API with no restart, its
# config_policy is visible in the §14 resolver, and the fresh tenant runs a REAL provider completion
# (MCI-001/TEN-003). A convenience alias for the CASE=second-tenant-provisioning case (PROVIDER=provider-one).
test-live-second-tenant:
	@PROVIDER=provider-one CASE=second-tenant-provisioning scripts/test/live-provider

verify: lint check-generated test-unit test-spikes check-spike-reports
	@bash scripts/verify/repository-boundary.sh
	@bash scripts/verify/foundation.sh

local-up:
	@test -x scripts/local/up || { echo "local stack not implemented" >&2; exit 2; }
	@scripts/local/up

local-down:
	@test -x scripts/local/down || { echo "local stack not implemented" >&2; exit 2; }
	@scripts/local/down

local-doctor:
	@test -x scripts/local/doctor || { echo "local doctor not implemented" >&2; exit 2; }
	@scripts/local/doctor

uat-local-live:
	@test -x scripts/uat/local-live || { echo "local live UAT not implemented" >&2; exit 2; }
	@RELEASE='$(RELEASE)' PROVIDER='$(PROVIDER)' scripts/uat/local-live

# E08 exit gate: the deterministic multi-client tier (always) + the live interactive journey
# (PROVIDER=provider-one, key from .env.local). uat-local-live above stays untouched.
uat-interactive:
	@test -x scripts/uat/interactive || { echo "interactive UAT not implemented" >&2; exit 2; }
	@PROVIDER='$(PROVIDER)' scripts/uat/interactive

# E09 exit gate: the deterministic coding journey (always) + the live coding journey
# (PROVIDER=provider-one, key + Git destination from .env.local). uat-local-live and
# uat-interactive above stay untouched.
uat-coding:
	@test -x scripts/uat/coding || { echo "coding UAT not implemented" >&2; exit 2; }
	@PROVIDER='$(PROVIDER)' scripts/uat/coding

# E10 exit gate: the deterministic/component/fault recovery core (always) + the named-but-gated live
# recovery smokes (PROVIDER=provider-one). The core is provider-agnostic — the kill is real, the provider
# is fake. uat-local-live / uat-interactive / uat-coding above stay untouched.
uat-recovery:
	@test -x scripts/uat/recovery || { echo "recovery UAT not implemented" >&2; exit 2; }
	@PROVIDER='$(PROVIDER)' scripts/uat/recovery

# E11 exit gate: the deterministic automation journey + scheduler fault + evidence-verify core (always) +
# the four already-registered live automation smokes (PROVIDER=provider-one). uat-local-live /
# uat-interactive / uat-coding / uat-recovery above stay untouched.
uat-automation:
	@test -x scripts/uat/automation || { echo "automation UAT not implemented" >&2; exit 2; }
	@PROVIDER='$(PROVIDER)' scripts/uat/automation

# E12 EXIT gate: the deterministic extensibility journey + hook fault + evidence-verify core (always) + the
# eight already-registered live extensibility smokes (PROVIDER=provider-one). The core is provider-agnostic —
# the extension crash is a real process kill, the provider is fake. uat-local-live / uat-interactive /
# uat-coding / uat-recovery / uat-automation above stay untouched.
uat-extensibility:
	@test -x scripts/uat/extensibility || { echo "extensibility UAT not implemented" >&2; exit 2; }
	@PROVIDER='$(PROVIDER)' scripts/uat/extensibility

# E13 EXIT gate: the managed-cloud catalog + committed-bundle evidence-verify core (always, no Docker) + the
# live tier (PROVIDER=provider-one) — the restart-less SPINE journey on ONE in-proc process (provision a
# tenant over the public API -> real provider run -> steer -> list -> cross-tenant deny, restart_count=0) plus
# the per-task MCI-00N smokes (secret/artifact/budget/route) each proven live in their own process. Ends in a
# REAL provider run. uat-local-live / uat-interactive / uat-coding / uat-recovery / uat-automation /
# uat-extensibility above stay untouched.
uat-managed-cloud:
	@test -x scripts/uat/managed-cloud || { echo "managed-cloud UAT not implemented" >&2; exit 2; }
	@PROVIDER='$(PROVIDER)' scripts/uat/managed-cloud

# E14 EXIT gate (SH-0 single-node alpha): the self-host catalog + committed-bundle evidence-verify core
# (always, no Docker) + the live tier (PROVIDER=provider-one) — the whole production-compose journey on two
# isolated stacks (clean install -> production bring-up -> CA-verified TLS edge -> config validate + doctor v2
# -> admin CLI provisioning through the edge -> a REAL provider run through the edge -> metrics/alert probe ->
# backup -> restore into a SEPARATE clean stack + restore verify -> support-bundle, restart_count=0). Ends in a
# REAL provider run. uat-local-live / uat-interactive / uat-coding / uat-recovery / uat-automation /
# uat-extensibility / uat-managed-cloud above stay untouched.
uat-self-host:
	@test -x scripts/uat/self-host || { echo "self-host UAT not implemented" >&2; exit 2; }
	@PROVIDER='$(PROVIDER)' scripts/uat/self-host

# E15 T3 — restricted Helm chart render/policy asserts: helm lint + Go asserts over `helm template`
# (ZERO ClusterRole, restricted securityContext, NetworkPolicy default-deny, PDB, migration Job hook,
# external-PG/S3-only) + kubeconform schema validation. Deterministic, needs no cluster; skips a
# check whose binary (helm/kubeconform) is absent.
uat-kubernetes:
	go test ./tests/uat/kubernetes/ -count=1

# E15 T3 — kind install smoke: `kind load` the images, `helm install`, the migration Job hook
# completes, /healthz green, provision via the admin CLI, enroll the E14 runner package from the host,
# a fake-provider run completes. NOT gated in `make verify` (Docker + kind bound). HONEST CEILING:
# kindnet does NOT enforce NetworkPolicy — enforcement is the operator leg (§6).
uat-kind:
	@command -v kind >/dev/null 2>&1 || { echo "kind not installed (brew install kind)" >&2; exit 2; }
	bash tests/uat/kubernetes/kind-smoke.sh

evidence-verify:
	@test -x scripts/evidence/verify || { echo "evidence verifier not implemented" >&2; exit 2; }
	@RELEASE='$(RELEASE)' scripts/evidence/verify
