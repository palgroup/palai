SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

.PHONY: \
	bootstrap generate check-generated lint test-unit test-component test-e2e \
	test-fault test-security test-performance test-live-provider test-live-hook-deny test-live-tenancy test-live-second-tenant test-live-run-history test-live-mac test-live-concurrency test-live-ios-demo test-spikes evidence-spikes \
	check-spike-reports verify local-up local-down local-doctor uat-local-live \
	uat-interactive uat-coding uat-recovery uat-automation uat-extensibility uat-managed-cloud uat-self-host \
	uat-kubernetes uat-kind uat-sh2 uat-sdk-parity uat-extensions uat-stable-release uat-wiring uat-wiring-live \
	uat-agent-surface uat-agent-surface-live uat-tools-memory uat-tools-memory-live uat-code-and-ship uat-code-and-ship-live \
	uat-tool-approval uat-tool-approval-live uat-fleet uat-fleet-live \
	uat-admin-console uat-admin-console-live uat-background \
	uat-fleet-console uat-fleet-console-live \
	uat-escape evidence-verify promote migration-resume-drill upgrade-drill \
	release-matrix-smoke provenance-offline-verify

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
# The PYTHON SDK rides it for the same reason and it did not until now. Its suite has existed the
# whole time — `ls sdks/python/tests/*.py | wc -l` → 7 on 2026-07-31 — and NO make target, no
# scripts/test tier and no workflow ever invoked it. That is this repository's signature failure in
# its purest form: the tests are not missing from the tree, they are missing from every invocation,
# and reading either the tree or the tier list finds nothing wrong. `uv` is already warmed by
# bootstrap, and the conformance runner under sdks/python/conformance is a DIFFERENT thing — it
# proves the shared corpus, not this surface.
	@uv run --locked --project sdks/python pytest sdks/python/tests -q

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

# E15 T2 live N->N+1 upgrade drill (OPS-005/007/008): two REAL builds (fork-point N + current N+1), an
# active fake run survives `palai upgrade` on its pinned engine, the alias rolls for new runs, ONE
# real-provider smoke (credential from .env.local), then `palai upgrade rollback` runs N on the expanded
# schema and an old-stamp runner is rejected with the hop message. One stack at a time; 0 leaks on exit.
# Not part of verify (Docker-bound + heavy).
upgrade-drill:
	@bash scripts/test/upgrade-drill.sh

test-e2e:
	@test -x scripts/test/e2e || { echo "end-to-end suite not implemented" >&2; exit 2; }
	@scripts/test/e2e

test-fault:
	@test -x scripts/test/fault || { echo "fault suite not implemented" >&2; exit 2; }
	@scripts/test/fault

test-security:
	@test -x scripts/test/security || { echo "security suite not implemented" >&2; exit 2; }
	@scripts/test/security

# E18 Task 6 performance tier (PER-001..004). Docker- and server-bound, so it is out of `verify`.
# Select with `make test-performance TEST=<negatives|service|sandbox|all>`. Every number is a
# macOS + Docker-Desktop number with NO SLO or reference-hardware claim; a run that cannot stamp its
# hardware/load profile produces no result at all.
test-performance:
	@test -x scripts/test/performance || { echo "performance suite not implemented" >&2; exit 2; }
	@scripts/test/performance

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

# E22 T1 live leg: the agent drives THIS Mac's own simulator — boot, wait for the accessibility tree,
# tap, screenshot, record — and every verb is an argv through palai.workspace.shell. No provider and no
# stack: the thing under test is the HOST. It needs no Docker, because a Mac deployment runs the control
# plane natively. Each leg skips by the NAME of the variable it is missing (PALAI_SIMULATOR_UDID for the
# simulator legs, PALAI_IOS_PROJECT + PALAI_IOS_SCHEME for the build/test leg).
test-live-mac:
	@go test -tags=live -count=1 -v -timeout 20m -run 'TestLiveMacHost' ./apps/control-plane/internal/execution/tools/live

# CAN ONE MAC RUN MORE THAN ONE SESSION AT A TIME? The product requirement for renting Macs, and the one
# thing no other suite proves: they all prove runs COMPLETE, which a serial stack does too. This reads the
# durable run ledger and fails unless two runs were IN FLIGHT AT THE SAME INSTANT on two different dispatch
# workers. It needs a running stack (`palai up --native`) and SKIPS with none; the whole package runs, so
# there is no -run allow-list for a new case to fall out of.
#
# Bring the stack up with BOTH knobs above 1 or this is red BY DESIGN — that is the regression it fences:
#   PALAI_DISPATCH_WORKERS=2 PALAI_RUNNER_CONCURRENCY=2 PALAI_WORKSPACE_ROOT=<abs> palai up --native
test-live-concurrency:
	@go test -tags=live -count=1 -v -timeout 12m ./tests/live/concurrency/

# THE iOS DEMO CHAIN, PROVEN AS A PRODUCT RATHER THAN AS A SUITE — clone a real public repo, run a shell
# on it, propose the change, and (optionally) build the app and show a screenshot.
#
# ‼️ IT IS HERE BECAUSE IT WAS THE ONLY LIVE PROOF IN THIS TREE WITH NO INVOCATION. scripts/live/ holds
# exactly one file and twelve sibling live proofs above have a target; this one was reachable only by
# walking the directory, which for the smoke that answers "can a person point the demo at a repository
# and watch it work" is the wrong kind of hidden. Measured 2026-08-06: `git grep` found no reference to
# it anywhere in the tree.
#
# It needs a running stack (PALAI_BASE_URL / PALAI_API_KEY, defaulted from .palai) and prints UNPROVEN
# rather than passing for a leg it cannot run — a smoke that blurred those two would convert a missing
# capability into a green line. Extra legs are opt-in:
#   ARGS=--with-build     also builds the app and shows a screenshot
#   ARGS=--with-accounts  also proves the per-session uid (needs palai-agentd installed:
#                         `sudo palai agentd install`, once, on this Mac)
test-live-ios-demo:
	@bash scripts/live/ios-demo-smoke $(ARGS)

verify: lint check-generated test-unit test-spikes check-spike-reports
	@bash scripts/verify/repository-boundary.sh
	@bash scripts/verify/foundation.sh
# The Docker-FREE half of the SEC-102 escape suite (E18 T7): the SAN coverage guard, the report
# redactor, and the not-attempted gate. `make verify` never compiles the `security` tag otherwise, so
# a new SAN case or a renamed proof broke no routine gate until someone ran `make uat-escape`.
# TestSandboxEscapeSuite skips itself without PALAI_ESCAPE_SUITE=1, so this stays Docker-free.
	@go test -tags=security -count=1 ./tests/uat/escape/

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

# E15 T6 EXIT gate (SH-2 RC): the SH-2 evidence anchor + catalog + verifier/promote units (always, no Docker) +
# the committed self-host-0.2.0 bundle through the shipped verifier + the mechanical promote gate (rc PASS /
# stable REFUSED). The live tier (PROVIDER=provider-one) drives the whole upgrade + DR + air-gap + helm journey
# one stack at a time, ending in a REAL provider run. HONEST CEILING (plan §6): the local seam is two-local-build
# upgrade, a kind cluster, an internal-network air-gap, and a same-host DR — the real cluster/air-gap/second-site
# legs are §6 operator legs and RC-beyond promote awaits §6 legs 1, 2 and 5.
uat-sh2:
	@test -x scripts/uat/sh2 || { echo "sh2 UAT not implemented" >&2; exit 2; }
	@PROVIDER='$(PROVIDER)' scripts/uat/sh2

# E16 EXIT gate (SDK parity + provider completeness, plan §T8 — the capstone): the SDK-parity evidence anchor +
# catalog + verifier/promote units (always, no Docker) + the two-provider runtime conformance + the shared-corpus
# cross-language equality + the committed sdk-provider-parity-0.1.0 bundle through the shipped verifier + the
# mechanical promote gate (rc PASS / stable REFUSED). The live tier (PROVIDER=provider-one) drives journey 63.1 —
# a model connection over the T1 API, a Responses run from ALL FOUR clients (three SDKs + the CLI) whose
# normalized decodes are mechanically diffed EQUAL, a restart with retained retrieval, and the gateway-off leg
# (the stand-in gateway killed, direct routes still serving) — ending in REAL provider-one + provider-two runs.
# HONEST CEILING (plan §6): the stand-in gateway is a local proxy (a real LiteLLM/private-server drill is §6);
# the typed-410 SDK surface is proven DETERMINISTICALLY (the corpus gone-410 + API-015), not in the journey;
# published npm/PyPI/Go-proxy releases are E18.
uat-sdk-parity:
	@test -x scripts/uat/sdk-parity || { echo "sdk-parity UAT not implemented" >&2; exit 2; }
	@PROVIDER='$(PROVIDER)' scripts/uat/sdk-parity

# E17 EXIT gate (stable extensions, plan §T11 — the crown): the Docker-free CORE (the E17 catalog gate over all
# 38 cases, the CapabilityTierProof anti-fabrication anchor's nine negatives, the committed extensions-0.1.0
# bundle through the shipped verifier, the promote gate's rc-PASS / no-tier-table-REFUSED / QUA-003-red-REFUSED /
# beyond-rc-REFUSED, and the SHIPPED /v1/capabilities map asserted BIT-EQUAL to the tier recompute) plus the
# CO-RUN of the suites that BACK the capabilities it flips (Slack + A2A packages and the console playwright specs
# with no Docker; the knowledge / workers / automation component suites IN FULL against a throwaway Postgres,
# which is where the three §T11 journeys live) and a PROVIDER-gated single-step live smoke. The co-run is what
# makes the bundle's AUTHORED per-case status honest: a red backing suite fails this target (proven by
# tests/uat/extensions TestARedBackingSuiteFailsTheGate). SKIP_JOURNEYS=1 runs the Docker-free core alone.
# HONEST CEILING (plan §6): there is NO real Slack workspace, NO foreign A2A peer, NO broker PRODUCT, NO real
# control-plane upstream behind the console, NO Apple signing material and NO vector store here — slack, a2a,
# queues and console close PREVIEW; knowledge-vector + apple-build close DISABLED; only knowledge and
# capability-workers close STABLE, computed from the claim outcomes rather than declared. This target claims none
# of those legs.
uat-extensions:
	@test -x scripts/uat/extensions || { echo "extensions UAT not implemented" >&2; exit 2; }
	@PROVIDER='$(PROVIDER)' SKIP_JOURNEYS='$(SKIP_JOURNEYS)' scripts/uat/extensions

# E18 T10 FINAL cross-epic EXIT gate (plan §T10 — the last gate in the program): the Docker-free CORE (the E18
# catalog gate over SEC-101..103 + PER-001..004, the ~50 anti-fabrication negatives, the committed
# release-1.0.0-rc1 bundle through the shipped verifier, the promote gate's rc-PASS / stable-REFUSED /
# leg-missing-REFUSED / SUP-3-REFUSED, the SHIPPED /v1/capabilities map asserted BIT-EQUAL to the CROSS-EPIC
# recompute, SEC-101's real tamper matrix, SEC-103's audit chain, and THE CROSS-EPIC RE-VERIFY of every
# committed bundle) plus a Docker-bound PER smoke with a mandatory profile and the SEC-103 component journey,
# plus a PROVIDER-gated single-step live run. SKIP_JOURNEYS=1 runs the Docker-free core alone;
# RUN_ESCAPE_SUITE=1 / RUN_MATRIX_SMOKE=1 add the two heavy opt-in legs.
#
# ITS OUTPUT IS AN SH-3 POSTURE REPORT, NOT A BLANKET "STABLE". HONEST CEILING (plan §6): NOT ONE §6 operator
# leg has been executed — no protected CI environment, no second maintainer, no registry/Sigstore/KMS
# credential, no reference hardware, no air-gap facility, no real Slack workspace, no foreign A2A peer, no
# broker product, no Apple signing material, no vector store. The local closure of this gate is an RC; SH-3
# Stable is the operator attestation the promote gate demands, naming all eleven legs one by one, and this
# target claims none of them.
uat-stable-release:
	@test -x scripts/uat/stable-release || { echo "stable-release UAT not implemented" >&2; exit 2; }
	@PROVIDER='$(PROVIDER)' SKIP_JOURNEYS='$(SKIP_JOURNEYS)' RUN_ESCAPE_SUITE='$(RUN_ESCAPE_SUITE)' RUN_MATRIX_SMOKE='$(RUN_MATRIX_SMOKE)' scripts/uat/stable-release

# E19 T9 EXIT gate (plan §T9 — the LAST gate of the integration-wiring epic): the Docker-free CORE (the
# committed integration-wiring-0.1.0 bundle through the shipped verifier, the 13-arm MOUNT-DERIVATION
# refusal matrix, the promote gate's rc-PASS / stable-REFUSED / mount-broken-REFUSED / tier-advanced-REFUSED,
# the credential-gated live inventory checked BOTH ways against the tests that exist, the pure adapter
# suites, and the SHIPPED /v1/capabilities map asserted bit-equal to the recompute) plus a Docker-bound
# JOURNEY (register a workspace over the shipped admin route -> a Socket Mode mention births a run -> the
# SAME event over HTTP births nothing -> an unauthorized click decides nothing -> an authorized click
# approves durably -> the run terminal commits an outbound delivery -> the pump delivers it exactly once ->
# an A2A push StreamResponse reaches a loopback sink -> the mounts are OBSERVED) plus the console specs.
# SKIP_JOURNEYS=1 runs the Docker-free core alone; RUN_CONSOLE_REAL=1 adds the REAL-upstream console profile.
#
# HONEST CEILING, and it is in the release's NAME (`integration-wiring`, not "slack verified"): NO real Slack
# workspace, NO foreign A2A peer, NO broker PRODUCT and NO deployed console with a screen-reader pass exist
# in this session. What it certifies is MOUNTED + CORRECT AGAINST THE PUBLISHED VENDOR CONTRACT + READY TO
# RUN UNCHANGED. NO TIER ADVANCES, and the promote gate refuses any bundle that says otherwise.
uat-wiring:
	@test -x scripts/uat/wiring || { echo "wiring UAT not implemented" >&2; exit 2; }
	@SKIP_JOURNEYS='$(SKIP_JOURNEYS)' RUN_CONSOLE_REAL='$(RUN_CONSOLE_REAL)' scripts/uat/wiring

# E19 T9 live tier: the ONE command the plan §0 handover promises. It runs every credential-gated leg in
# uat.WiringLiveLegs with NO CODE CHANGES — keys come from .env.local via `set -a`, never argv. A leg whose
# variable is absent SKIPS by name, quoting the §0 row that supplies it, so a PARTIAL handover reports
# partial-green instead of a red wall. READ THE SKIP LINES: a skip is not a pass.
uat-wiring-live:
	@test -x scripts/uat/wiring || { echo "wiring UAT not implemented" >&2; exit 2; }
	@RUN_LIVE=1 SKIP_JOURNEYS='$(SKIP_JOURNEYS)' RUN_CONSOLE_REAL='$(RUN_CONSOLE_REAL)' scripts/uat/wiring

# E20 T5 exit gate (plan §T5): the slack-agent-surface-0.1.0 bundle, the forgery-derivation refusal matrix,
# the promote gate, the four case ids this epic opened, and the Docker-bound journey. uat-wiring and every
# earlier uat-* target above stay untouched.
uat-agent-surface:
	@test -x scripts/uat/agent-surface || { echo "agent-surface UAT not implemented" >&2; exit 2; }
	@SKIP_JOURNEYS='$(SKIP_JOURNEYS)' scripts/uat/agent-surface

# THE ONE COMMAND (plan §T5): every credential-gated live leg in uat.WiringLiveLegs, E20's four included.
# Each SKIPS by the NAME of the variable it is missing, so a partial handover reports partial-green rather
# than a red wall, and no code change is needed to run any of them.
uat-agent-surface-live:
	@test -x scripts/uat/agent-surface || { echo "agent-surface UAT not implemented" >&2; exit 2; }
	@RUN_LIVE=1 SKIP_JOURNEYS='$(SKIP_JOURNEYS)' scripts/uat/agent-surface

# E21 T7 exit gate (plan §T7): the tools-memory-0.1.0 bundle, the refusal matrix (a persisted surface that
# CONTAINS a search result; an answer carrying a mention the MODEL chose), the promote gate dispatched ahead
# of E20's, the five case ids this epic opened, and the Docker-bound journey. Every earlier uat-* target above
# stays untouched.
uat-tools-memory:
	@test -x scripts/uat/tools-memory || { echo "tools-memory UAT not implemented" >&2; exit 2; }
	@SKIP_JOURNEYS='$(SKIP_JOURNEYS)' scripts/uat/tools-memory

# THE ONE COMMAND (plan §T7): every credential-gated live leg in uat.WiringLiveLegs, unchanged. Each SKIPS by
# the NAME of the variable it is missing, so a partial handover reports partial-green rather than a red wall,
# and no code change is needed to run any of them. E21's §6 contribution is to make leg 1 BIGGER again — it
# now covers the tool surface, the workspace search and the mention — and to add leg 2's four M20 measurements
# at zero extra cost.
uat-tools-memory-live:
	@test -x scripts/uat/tools-memory || { echo "tools-memory UAT not implemented" >&2; exit 2; }
	@RUN_LIVE=1 SKIP_JOURNEYS='$(SKIP_JOURNEYS)' scripts/uat/tools-memory

# E22 T7 exit gate (plan §T7): the code-and-ship-0.1.0 bundle, the refusal matrix (a DENIED publication
# marked published; a publish schema carrying a `base` property; a ticket body whose tool name turns up in
# the advertised set), the promote gate dispatched ahead of E21's, the five case ids this epic opened, and
# the Docker-bound backing co-run. Every earlier uat-* target above stays untouched.
uat-code-and-ship:
	@test -x scripts/uat/code-and-ship || { echo "code-and-ship UAT not implemented" >&2; exit 2; }
	@SKIP_JOURNEYS='$(SKIP_JOURNEYS)' scripts/uat/code-and-ship

# THE ONE COMMAND (plan §T7): every credential-gated live leg in uat.WiringLiveLegs, plus this epic's four
# roots — tests/live/repository, tests/live/workspace, tests/live/code-and-ship and the Mac host legs. Each
# SKIPS by the NAME of the variable it is missing, so a partial handover reports partial-green rather than a
# red wall, and no code change is needed to run any of them. E22's §6 contribution is to make leg 1 BIGGER
# again — it now covers repository cloning, an approved push, a draft pull request and a file upload — and to
# OPEN leg 5, a real GitHub App, which this epic does not close.
uat-code-and-ship-live:
	@test -x scripts/uat/code-and-ship || { echo "code-and-ship UAT not implemented" >&2; exit 2; }
	@RUN_LIVE=1 SKIP_JOURNEYS='$(SKIP_JOURNEYS)' scripts/uat/code-and-ship

# E23 T7 exit gate (plan §T7): the tool-approval-0.1.0 bundle, the refusal matrix (the MCP server's own
# `description` rendered onto the approval screen; a denied call marked as executed; a run left waiting after
# its approval expired; a merge tool that grew a `pull_request_number`), the promote gate dispatched ahead of
# E22's, the five case ids this epic opened, and the Docker-bound backing co-run. Every earlier uat-* target
# above stays untouched.
uat-tool-approval:
	@test -x scripts/uat/tool-approval || { echo "tool-approval UAT not implemented" >&2; exit 2; }
	@SKIP_JOURNEYS='$(SKIP_JOURNEYS)' scripts/uat/tool-approval

# THE ONE COMMAND (plan §T7): the credential-gated live legs — a real approval message posted into a real
# Slack thread, the Atlassian MCP reachability leg, and an opt-in real merge (PALAI_GIT_LIVE_MERGE) that asks
# github.com for its own answer to a moved head. Each SKIPS by the NAME of the variable it is missing, so a
# partial handover reports partial-green rather than a red wall, and no code change is needed to run any of
# them. E23's §6 contribution is to make leg 1 BIGGER again — it now covers the approval message, the modal,
# an unauthorized click and a merge — and to OPEN leg 6, an approval expiring in REAL time, which this epic
# does not close.
uat-tool-approval-live:
	@test -x scripts/uat/tool-approval || { echo "tool-approval UAT not implemented" >&2; exit 2; }
	@RUN_LIVE=1 SKIP_JOURNEYS='$(SKIP_JOURNEYS)' scripts/uat/tool-approval

# E24 T8 exit gate (plan §T8): the runner-fleet-0.1.0 bundle, the refusal matrix (an offer that crossed a pool
# or a tenant boundary; a run dead-lettered for an empty pool; a renewal refused after a key revocation; a
# revoked machine that came back after a restart; a registry with no shared label two identities came in
# under), the promote gate dispatched ahead of E23's, the §3.6 D4 belief sweep, the reachability sweep over
# E24's new exported surface, the five case ids this epic opened, and the Docker-bound backing co-run. Every
# earlier uat-* target above stays untouched.
uat-fleet:
	@test -x scripts/uat/fleet || { echo "fleet UAT not implemented" >&2; exit 2; }
	@SKIP_JOURNEYS='$(SKIP_JOURNEYS)' scripts/uat/fleet

# THE ONE COMMAND (plan §T8) — and for this epic it is the one that has to be honest about adding NOTHING.
# E24 opens no live root of its own: its §6 legs want a SECOND PHYSICAL MACHINE, a real pool-key revocation in
# real time, and a `linux/amd64` host, and no credential produces any of those. So this runs E23's live set
# unchanged, and E24's §6 contribution is to make leg 1 BIGGER again (a real remote enrolment, a real pool-key
# revocation, a real machine revocation across a restart) while closing none of it.
uat-fleet-live:
	@test -x scripts/uat/fleet || { echo "fleet UAT not implemented" >&2; exit 2; }
	@RUN_LIVE=1 SKIP_JOURNEYS='$(SKIP_JOURNEYS)' scripts/uat/fleet

# THE ONE COMMAND (plan §T9) — the E25 admin-console exit gate. Docker-free core (the console TYPECHECK,
# which §3.6 D15 measured hits no other gate in this tree; the bundle; the refusal matrix; the promote gate
# dispatched ahead of E24's and E23's; the CON- catalog and orphan sweep; the reachability sweep over E25's
# new exported surface and the guard that every component test this epic added is NAMED in
# scripts/test/component's -run allow-list; the ciphertext query pin and the environment projection pin; the
# automation and operator-doc corpora; and the console specs on the FAKE profile in BOTH colour schemes),
# then the Docker-bound journey tier (the tenancy corpus — 000046 opened two tenant tables — plus the
# component-real legs where E25's Go seams live). Every earlier uat-* target above stays untouched.
uat-admin-console:
	@test -x scripts/uat/admin-console || { echo "admin-console UAT not implemented" >&2; exit 2; }
	@SKIP_JOURNEYS='$(SKIP_JOURNEYS)' scripts/uat/admin-console

# The REAL-UPSTREAM tier, and for this epic that is what "live" means: E25 has no credential-gated leg at all
# — there is no vendor to call — so the honest second command runs the SAME spec files against a compose
# control plane and then the fake-vs-real conformance sweep. THAT SWEEP IS THE ONLY THING THAT RE-OBSERVES
# THE FIVE REPAIRED LEDGER ROWS (§6 leg 2), which is why it is a target rather than a note. It needs a
# running stack: `PALAI_DISPATCH_WORKERS=1 PALAI_MODEL_PROVIDER=fake palai local up`, plus PALAI_BASE_URL and
# PALAI_API_KEY exported. The sweep refuses to skip without them.
uat-admin-console-live:
	@test -x scripts/uat/admin-console || { echo "admin-console UAT not implemented" >&2; exit 2; }
	@RUN_CONSOLE_REAL=1 SKIP_JOURNEYS='$(SKIP_JOURNEYS)' scripts/uat/admin-console

# THE ONE COMMAND (plan §T7) — the E26 exit gate. Docker-free core plus a Docker-bound journey tier; no
# credential anywhere, because this epic has no vendor to call. SKIP_JOURNEYS=1 opts out of the tier that
# needs a throwaway Postgres and a real daemon, and the script SAYS SO in its closing line rather than
# implying a tier that did not run.
#
# THERE IS NO `uat-background-live` AND ITS ABSENCE IS THE HONEST ANSWER. E26's §6 legs want a real
# xcodebuild on a real Mac with its duration written down, a control-plane restart through a real service
# manager, an overnight orphan hunt, the real rate of pgid reuse on a Mac, an observed credential exposure,
# and an operator watching a PALAI_DISPATCH_WORKERS=0 stack land nothing. Not one of those is produced by a
# credential, so a "live" target would be a second name for the same run.
uat-background:
	@test -x scripts/uat/background || { echo "background UAT not implemented" >&2; exit 2; }
	@SKIP_JOURNEYS='$(SKIP_JOURNEYS)' scripts/uat/background

# THE ONE COMMAND (plan §T4) — the E28 fleet-console exit gate. Docker-free core (the console TYPECHECK; the
# bundle; the refusal matrix, whose every negative moves ONE ledger row and leaves the declared counter where
# it was; the promote gate dispatched ahead of E26's; the FLC- catalog and the orphan sweep in three
# directions; the SOURCE sweeps that re-derive the route ledger from lib/routes.ts, every destructive action's
# confirmation from the page sources and every ceiling gap id from the page that states it; the reachability
# sweep over E28's new exported surface; the automation and operator-doc corpora; and the console specs on the
# FAKE profile in BOTH colour schemes), then the Docker-bound journey tier (the tenancy corpus — E28 opened no
# migration but DID open two write routes on a tenant table — plus the component-real legs where T1's birth
# path lives). Every earlier uat-* target above stays untouched.
uat-fleet-console:
	@test -x scripts/uat/fleet-console || { echo "fleet-console UAT not implemented" >&2; exit 2; }
	@SKIP_JOURNEYS='$(SKIP_JOURNEYS)' scripts/uat/fleet-console

# The REAL-UPSTREAM tier, and for this epic that is what "live" means: E28 has no credential-gated leg at all
# — there is no vendor to call, and the one thing a credential WOULD buy (a rented Mac) is §6 leg 1, which
# this epic does not close. So the honest second command runs the SAME spec files against a compose control
# plane and then the fake-vs-real conformance sweep, WHICH IS THE TIER THAT PRODUCES GROUP (h): the sweep's
# compared-collection membership, which E28 T3 raised from thirteen to sixteen. It needs a running stack:
# `PALAI_DISPATCH_WORKERS=1 PALAI_MODEL_PROVIDER=fake palai local up`, plus PALAI_BASE_URL and PALAI_API_KEY
# exported. The sweep refuses to skip without them.
uat-fleet-console-live:
	@test -x scripts/uat/fleet-console || { echo "fleet-console UAT not implemented" >&2; exit 2; }
	@RUN_CONSOLE_REAL=1 SKIP_JOURNEYS='$(SKIP_JOURNEYS)' scripts/uat/fleet-console

# E18 T1 image half of the release matrix (Docker-bound, so NOT in `make verify` — like uat-kind): builds
# linux/amd64 + linux/arm64 for all three images, asserts each indexed tar's digest/image_id/arch against the
# artifact's own bytes, and boot-smokes the amd64 tar (emulated on an arm64 host). The binary half is covered
# Docker-free by `go test ./scripts/release/...`. Everything it tags is removed on exit; the build cache is
# left intact. HONEST CEILING: a boot-smoke is not a full UAT run on amd64 (plan §6 leg 3).
release-matrix-smoke:
	@bash scripts/release/matrix-smoke.sh

# E18 T3 offline half (Docker-bound, so NOT in `make verify` — like release-matrix-smoke): runs all four
# provenance-verify legs inside a container with NO network device. `go test ./scripts/release/...` SKIPS
# that leg, so without this target the offline claim is UNPROVEN. Needs an already-loaded openssl+python3
# image; the reference engine qualifies. That image has no git, so it also exercises leg (4)'s loud
# GIT ABSENT degradation.
PROVENANCE_TOOL_IMAGE ?= palai/reference-engine:local
provenance-offline-verify:
	@docker image inspect '$(PROVENANCE_TOOL_IMAGE)' >/dev/null 2>&1 || { echo "provenance-offline-verify: load $(PROVENANCE_TOOL_IMAGE) first, or set PROVENANCE_TOOL_IMAGE=<an openssl+python3 image>" >&2; exit 2; }
	@PALAI_PROVENANCE_TOOL_IMAGE='$(PROVENANCE_TOOL_IMAGE)' go test -count=1 -run TestProvenanceOfflineVerifyNetworkNone ./scripts/release/...

# E18 T7 SEC-102 escape suite (Docker-bound, so NOT in `make verify` — like release-matrix-smoke): runs the
# EXISTING SAN corpus (SAN-001..008 + the SAN-011 negatives) as ONE pass with ONE report, plus the added
# finding->quarantine behaviour arm. It invents no escape class. HONEST CEILING (also written into the
# report): this is the LOCAL OCI seam — the microVM / managed high-isolation path is managed-scope, not
# claimed, and kernel-exploit research is out of scope; the suite proves DENIAL + QUARANTINE mechanics.
uat-escape:
	@PALAI_ESCAPE_SUITE=1 go test -tags=security -count=1 -timeout 40m -v ./tests/uat/escape/

# E15 T6 promote gate: refuse to tag a release without a rollback + restore proof (plan §7). Default target rc;
# `make promote RELEASE=<name> TO=stable` gates a stable promote (awaits the E14 §6 operator-leg attestation).
promote:
	@test -x scripts/release/promote.sh || { echo "promote gate not implemented" >&2; exit 2; }
	@RELEASE='$(RELEASE)' scripts/release/promote.sh '$(TO)'
