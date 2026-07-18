SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

.PHONY: \
	bootstrap generate check-generated lint test-unit test-component test-e2e \
	test-fault test-security test-live-provider test-spikes evidence-spikes \
	check-spike-reports verify local-up local-down local-doctor uat-local-live

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
	@scripts/uat/local-live
