# Palai Contract Spine (E02) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `MASTER-SPEC.md` E02 kapsamını üretmek: canonical JSON Schema 2020-12 sözleşmeleri, OpenAPI 3.2 + AsyncAPI 3.1 üretimi, generator-compatible projection'ın semantic diff doğrulaması, cross-language fixture corpus'u ve yedi execution resource'unun pure-function state transition tabloları.

**Architecture:** Canonical şemalar `protocols/schemas/` altında elle yazılır; `scripts/contracts/generate` tek deterministic pipeline ile Go types (`packages/contracts/`), TypeScript/Python fixture types (`protocols/generated/`), OpenAPI 3.1.2 projection ve semantic check üretir. State tabloları `packages/state-machines/` içinde saf Go'dur; database, HTTP, provider veya Docker import etmez. Event isimleri tek registry dosyasından gelir ve tablolarla conformance testinde kesişir.

**Tech Stack:** Go 1.26.4 (module `github.com/palgroup/palai`), JSON Schema 2020-12, OpenAPI 3.2 + 3.1.2 projection, AsyncAPI 3.1, pnpm/tsc ve Python 3.14 (corpus round-trip checks), spike'tan terfi eden in-house template generator (`spikes/contracts/generator`).

## Global Constraints

- **Execution gate:** E01 ADR-0001..0005 accepted olmadan Task 1 başlamaz. ADR-0002 contract-toolchain kararı spike baseline'ından (in-house generator + tsc + datamodel-code-generator sınıfı araçlar) saparsa önce bu plan mekanik güncellenir ve review edilir.
- Canonical spec asla OpenAPI 3.0'a düşürülmez; projection mekanik üretilir ve semantic diff ile doğrulanır (master plan §4.3).
- Schema `$id` kökü: `https://schemas.palai.dev/`. Tüm timestamps RFC 3339 UTC.
- ID'ler opak, prefix'li, URL-safe (`^<prefix>_[A-Za-z0-9_-]+$`); client ID içinden bilgi parse edemez (spec §20.3).
- Unknown object fields ignore edilir ve korunur; enum'lar open-enum'dur (spec §20.6 → UAT API-009).
- Generated dosyalar kaynak dosyalarla aynı commit'tedir; ikinci `make generate` çalışması zero diff üretir; drift `make check-generated` ile CI blocker'dır.
- `packages/contracts` ve `packages/state-machines` yalnızca Go stdlib import eder (master plan §5.1 dependency direction). Handwritten duplicate public type yasaktır.
- Bu epic'te API handler/business logic yazılmaz (E02 exit gate).
- Her task: önce failing test, minimum implementation, tek amaçlı commit; task sonunda `make verify` yeşil.
- API-Version tarihi: `2026-07-16`.

## local-live-proof eşlemesi

Bu plan `2026-07-16-local-live-proof.md`'nin iki task'ının süperseti:

| local-live-proof | Bu plandaki karşılığı |
|---|---|
| Task 2 (minimum schemas + generate/check) | Task 1–8 |
| Task 4 (run/attempt/tool_call/response tabloları) | Task 9–12 |

Yürütme sırası: local-live-proof Task 1 (toolchain reconciliation) → bu planın tamamı → local-live-proof Task 3'ten devam. Bu plan bittiğinde local-live-proof Task 2/4 checkbox'ları "phase-02 ile karşılandı" notu ve commit referansıyla işaretlenir; `packages/state-machines.Apply` imzası oradaki tanımla bire birdir.

---

### Task 1: Deterministic contract pipeline ve opaque ID seed schema

**Files:**

- Create: `scripts/contracts/generate`
- Create: `scripts/contracts/check`
- Create: `scripts/contracts/generator/` (spike'tan terfi: `spikes/contracts/generator/`)
- Create: `protocols/schemas/common/id.json`
- Test: `tests/conformance/contracts/pipeline_test.go`

**Interfaces:**

- Produces: `make generate` → `packages/contracts/*.go`, `protocols/generated/typescript/*.ts`, `protocols/generated/python/*.py`; `make check-generated` → drift'te non-zero exit. Sonraki tüm task'lar bu pipeline'a schema ekler.

- [ ] **Step 1: Failing determinism/drift testini yaz**

```go
package contracts_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(bytes.TrimSpace(out))
}

func runMake(t *testing.T, target string) error {
	t.Helper()
	cmd := exec.Command("make", target)
	cmd.Dir = repoRoot(t)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func TestGenerateIsDeterministic(t *testing.T) {
	if err := runMake(t, "generate"); err != nil {
		t.Fatalf("first generate: %v", err)
	}
	first := hashTree(t, filepath.Join(repoRoot(t), "packages/contracts"))
	if err := runMake(t, "generate"); err != nil {
		t.Fatalf("second generate: %v", err)
	}
	second := hashTree(t, filepath.Join(repoRoot(t), "packages/contracts"))
	if first != second {
		t.Fatal("generate is not deterministic")
	}
}

func TestCheckGeneratedFailsOnDrift(t *testing.T) {
	root := repoRoot(t)
	target := filepath.Join(root, "packages/contracts/ids.gen.go")
	original, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.WriteFile(target, original, 0o644) })
	if err := os.WriteFile(target, append(original, []byte("\n// drift\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runMake(t, "check-generated"); err == nil {
		t.Fatal("check-generated must fail on drift")
	}
}
```

`hashTree` aynı dosyada: dizini deterministic sırayla yürüyüp SHA-256 toplar (`filepath.WalkDir` + `sort` + `crypto/sha256`).

- [ ] **Step 2: Fail'i doğrula**

Run: `go test ./tests/conformance/contracts -run 'TestGenerate|TestCheckGenerated' -v`

Expected: FAIL — `scripts/contracts/generate` yok, `make generate` exit 2 ("contracts capability not implemented").

- [ ] **Step 3: Generator'ı spike'tan terfi ettir ve seed schema'yı yaz**

```bash
cp -R spikes/contracts/generator scripts/contracts/generator
```

`scripts/contracts/generator/main.go` içindeki input/output kökleri değiştirilir: input `protocols/schemas/`, output eşlemesi Go → `packages/contracts/` (package adı `contracts`, dosya suffix `.gen.go`), TypeScript → `protocols/generated/typescript/`, Python → `protocols/generated/python/`. Şablonlardaki spike fixture adları schema başına dosya üretecek şekilde parametrize edilir.

`scripts/contracts/generate` (executable):

```bash
#!/usr/bin/env bash
set -euo pipefail
root="$(git rev-parse --show-toplevel)"
go run "$root/scripts/contracts/generator" -schemas "$root/protocols/schemas" \
  -go-out "$root/packages/contracts" \
  -ts-out "$root/protocols/generated/typescript" \
  -py-out "$root/protocols/generated/python"
```

`scripts/contracts/check` (executable): `generate`'i geçici dizine üretip `git diff --exit-code`/`diff -r` ile mevcut generated çıktıyla karşılaştırır; fark varsa dosya listesiyle non-zero döner.

`protocols/schemas/common/id.json`:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://schemas.palai.dev/common/id.json",
  "title": "CanonicalIdentifiers",
  "$defs": {
    "opaque_id": {"type": "string", "pattern": "^[a-z][a-z0-9]{1,11}_[A-Za-z0-9_-]+$"},
    "organization_id": {"type": "string", "pattern": "^org_[A-Za-z0-9_-]+$"},
    "project_id": {"type": "string", "pattern": "^prj_[A-Za-z0-9_-]+$"},
    "session_id": {"type": "string", "pattern": "^ses_[A-Za-z0-9_-]+$"},
    "response_id": {"type": "string", "pattern": "^resp_[A-Za-z0-9_-]+$"},
    "run_id": {"type": "string", "pattern": "^run_[A-Za-z0-9_-]+$"},
    "attempt_id": {"type": "string", "pattern": "^att_[A-Za-z0-9_-]+$"},
    "tool_call_id": {"type": "string", "pattern": "^tcall_[A-Za-z0-9_-]+$"},
    "command_id": {"type": "string", "pattern": "^cmd_[A-Za-z0-9_-]+$"},
    "event_id": {"type": "string", "pattern": "^evt_[A-Za-z0-9_-]+$"},
    "artifact_id": {"type": "string", "pattern": "^art_[A-Za-z0-9_-]+$"},
    "workspace_id": {"type": "string", "pattern": "^wksp_[A-Za-z0-9_-]+$"},
    "message_id": {"type": "string", "pattern": "^msg_[A-Za-z0-9_-]+$"},
    "request_id": {"type": "string", "pattern": "^req_[A-Za-z0-9_-]+$"},
    "frame_id": {"type": "string", "pattern": "^frm_[A-Za-z0-9_-]+$"},
    "model_request_id": {"type": "string", "pattern": "^mreq_[A-Za-z0-9_-]+$"}
  }
}
```

- [ ] **Step 4: Generate + testleri çalıştır**

Run: `make generate && go test ./tests/conformance/contracts -run 'TestGenerate|TestCheckGenerated' -v`

Expected: PASS; `packages/contracts/ids.gen.go`, `protocols/generated/typescript/ids.ts`, `protocols/generated/python/ids.py` üretilmiş ve ikinci üretim zero diff.

- [ ] **Step 5: Commit**

```bash
git add scripts/contracts protocols/schemas protocols/generated packages/contracts tests/conformance/contracts
git commit -m "feat: promote the deterministic contract pipeline"
```

### Task 2: Problem Details, resource envelope ve pagination schemas

**Files:**

- Create: `protocols/schemas/common/problem.json`
- Create: `protocols/schemas/common/resource.json`
- Create: `protocols/schemas/common/pagination.json`
- Test: `tests/conformance/contracts/common_test.go`

**Interfaces:**

- Consumes: Task 1 pipeline, `id.json`.
- Produces: `contracts.Problem`, `contracts.ResourceEnvelope`, `contracts.Page` Go types; sonraki şemalar `problem.json` ve `resource.json`'a `$ref` verir.

- [ ] **Step 1: Failing schema testlerini yaz**

```go
func TestProblemRequiresStableCodeAndRequestID(t *testing.T) {
	// required: type,title,status,code,request_id — eksik alanlı fixture validate edilemez
}
func TestProblemStableCodesAreDocumented(t *testing.T) {
	// spec §20.10 tablosundaki 26 stable code problem.json $defs.known_codes listesinde birebir vardır
}
func TestResourceEnvelopeTimestampsAreRFC3339(t *testing.T)
func TestPageRequiresDataAndHasMore(t *testing.T)
```

Validation için test helper'ı `santhosh-tekuri/jsonschema` yerine pipeline'ın ürettiği Go types + `encoding/json` round-trip ve alan kontrolleri kullanır; harici validator dependency eklenmez.

- [ ] **Step 2: Fail'i doğrula**

Run: `go test ./tests/conformance/contracts -run 'TestProblem|TestResource|TestPage' -v`

Expected: FAIL — şemalar ve generated types yok.

- [ ] **Step 3: Şemaları yaz**

`problem.json` (RFC 9457, spec §20.10):

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://schemas.palai.dev/common/problem.json",
  "title": "Problem",
  "type": "object",
  "required": ["type", "title", "status", "code", "request_id"],
  "properties": {
    "type": {"type": "string"},
    "title": {"type": "string"},
    "status": {"type": "integer", "minimum": 100, "maximum": 599},
    "detail": {"type": "string"},
    "instance": {"type": "string"},
    "code": {"type": "string", "pattern": "^[a-z][a-z0-9_]*$"},
    "request_id": {"$ref": "id.json#/$defs/request_id"},
    "retryable": {"type": "boolean"},
    "field_errors": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["field", "code"],
        "properties": {"field": {"type": "string"}, "code": {"type": "string"}, "detail": {"type": "string"}},
        "additionalProperties": true
      }
    },
    "context": {"type": "object"}
  },
  "additionalProperties": true,
  "$defs": {
    "known_codes": {
      "enum": ["invalid_request", "invalid_state", "unsupported_content", "missing_idempotency_key",
        "authentication_required", "invalid_token", "expired_token",
        "permission_denied", "capability_denied", "policy_denied", "region_denied",
        "not_found", "revision_conflict", "idempotency_mismatch", "idempotency_in_progress",
        "active_run_conflict", "lease_conflict", "gone", "idempotency_result_expired", "retention_expired",
        "precondition_failed", "payload_too_large", "context_too_large",
        "schema_validation_failed", "unsupported_model_capability",
        "rate_limited", "quota_exceeded", "concurrency_exceeded",
        "internal_error", "provider_error", "tool_transport_error", "runner_error",
        "capacity_unavailable", "dependency_unavailable", "maintenance", "operation_timed_out"]
    }
  }
}
```

`code` open-enum kalır; `known_codes` dokümante liste olarak test edilir. `resource.json` (spec §20.4 — required `id`, `object`, `created_at`; `organization_id`, `project_id`, `updated_at`, `revision` ≥1, `metadata` maxProperties 64, `labels` string-map; `$defs.timestamp` = `{"type":"string","format":"date-time"}`). `pagination.json` `$defs`: `page_params` (`limit` 1..200 default 50, `after`, `before`) ve `page` (required `data`,`has_more`; `next_cursor`/`previous_cursor` nullable) — spec §20.7.

- [ ] **Step 4: Generate + testleri çalıştır**

Run: `make generate && go test ./tests/conformance/contracts -run 'TestProblem|TestResource|TestPage' -v`

Expected: PASS; zero drift.

- [ ] **Step 5: Commit**

```bash
git add protocols/schemas/common protocols/generated packages/contracts tests/conformance/contracts
git commit -m "feat: define common problem, resource, and pagination contracts"
```

### Task 3: Content items ve message schema

**Files:**

- Create: `protocols/schemas/execution/content.json`
- Create: `protocols/schemas/execution/message.json`
- Test: `tests/conformance/contracts/content_test.go`

**Interfaces:**

- Produces: `contracts.ContentItem`, `contracts.Message`; `response.json` ve `response-create.json` bunlara `$ref` verir.

- [ ] **Step 1: Failing testleri yaz**

```go
func TestKnownContentItemTypesValidate(t *testing.T)        // 14 tip için birer minimal fixture
func TestUnknownContentItemTypeRoundTripsPreserved(t *testing.T) // {"type":"holo_ref","x":1} kaybolmaz (API-009)
func TestMessageRoleIsOpenEnum(t *testing.T)                 // bilinmeyen role reddedilmez, korunur
```

- [ ] **Step 2: Fail'i doğrula**

Run: `go test ./tests/conformance/contracts -run TestContent -v` → FAIL (schema yok).

- [ ] **Step 3: Şemaları yaz**

`content.json` — base + bilinen tipler `if/then` ile (open union; `oneOf` kullanmak unknown tipi kırar, yasak):

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://schemas.palai.dev/execution/content.json",
  "title": "ContentItem",
  "type": "object",
  "required": ["type"],
  "properties": {"type": {"type": "string"}},
  "additionalProperties": true,
  "allOf": [
    {"if": {"properties": {"type": {"const": "input_text"}}}, "then": {"required": ["text"], "properties": {"text": {"type": "string"}}}},
    {"if": {"properties": {"type": {"const": "output_text"}}}, "then": {"required": ["text"], "properties": {"text": {"type": "string"}}}},
    {"if": {"properties": {"type": {"const": "image_ref"}}}, "then": {"required": ["artifact_id"], "properties": {"artifact_id": {"$ref": "../common/id.json#/$defs/artifact_id"}}}},
    {"if": {"properties": {"type": {"const": "audio_ref"}}}, "then": {"required": ["artifact_id"], "properties": {"artifact_id": {"$ref": "../common/id.json#/$defs/artifact_id"}}}},
    {"if": {"properties": {"type": {"const": "file_ref"}}}, "then": {"required": ["artifact_id"], "properties": {"artifact_id": {"$ref": "../common/id.json#/$defs/artifact_id"}}}},
    {"if": {"properties": {"type": {"const": "artifact_ref"}}}, "then": {"required": ["artifact_id"], "properties": {"artifact_id": {"$ref": "../common/id.json#/$defs/artifact_id"}}}},
    {"if": {"properties": {"type": {"const": "structured_json"}}}, "then": {"required": ["schema_name", "data"], "properties": {"schema_name": {"type": "string"}, "schema_version": {"type": "string"}, "data": {}}}},
    {"if": {"properties": {"type": {"const": "tool_request"}}}, "then": {"required": ["tool_call_id", "name", "arguments"], "properties": {"tool_call_id": {"$ref": "../common/id.json#/$defs/tool_call_id"}, "name": {"type": "string"}, "arguments": {"type": "object"}}}},
    {"if": {"properties": {"type": {"const": "tool_result"}}}, "then": {"required": ["tool_call_id", "status"], "properties": {"tool_call_id": {"$ref": "../common/id.json#/$defs/tool_call_id"}, "status": {"type": "string"}, "content": {}}}},
    {"if": {"properties": {"type": {"const": "refusal"}}}, "then": {"properties": {"reason": {"type": "string"}}}},
    {"if": {"properties": {"type": {"const": "warning"}}}, "then": {"required": ["message"], "properties": {"message": {"type": "string"}}}},
    {"if": {"properties": {"type": {"const": "citation"}}}, "then": {"required": ["source"], "properties": {"source": {"type": "string"}, "span": {"type": "object"}}}},
    {"if": {"properties": {"type": {"const": "compacted_context"}}}, "then": {"required": ["summary"], "properties": {"summary": {"type": "string"}, "replaced_sequences": {"type": "array", "items": {"type": "integer"}}}}},
    {"if": {"properties": {"type": {"const": "redacted_content"}}}, "then": {"properties": {"reason": {"type": "string"}}}}
  ]
}
```

`message.json`: required `id`, `role`, `content`, `created_at`; `role` open string (dokümante değerler: `user`, `assistant`, `tool`, `system_notice`, `external_actor`); `content` array of `content.json`; opsiyonel `source_ref`, `visibility`, `delivery` (`queue|steer|interrupt`).

- [ ] **Step 4: Generate + testler**

Run: `make generate && go test ./tests/conformance/contracts -run TestContent -v` → PASS.

- [ ] **Step 5: Commit**

```bash
git add protocols/schemas/execution protocols/generated packages/contracts tests/conformance/contracts
git commit -m "feat: define content item and message contracts"
```

### Task 4: Response create/response/usage schemas

**Files:**

- Create: `protocols/schemas/execution/response-create.json`
- Create: `protocols/schemas/execution/response.json`
- Create: `protocols/schemas/execution/usage.json`
- Test: `tests/conformance/contracts/response_test.go`

**Interfaces:**

- Produces: `contracts.ResponseCreateRequest`, `contracts.Response`, `contracts.Usage`. `response.json` required seti local-live-proof Task 2 ile birebir: `["id","object","status","created_at","model","output","usage"]`.

- [ ] **Step 1: Failing testleri yaz**

```go
func TestResponseFixtureRoundTripsWithoutLosingOmittedFields(t *testing.T) // omitted `error` null'a dönüşmez
func TestResponseCreateRejectsBothContinuationKeys(t *testing.T)          // previous_response_id + session_id birlikte geçersiz
func TestResponseStatusCoversSpecLifecycle(t *testing.T)                  // §8.3'teki 11 durum dokümante enum listesinde
func TestUsageRequiresTokenCounts(t *testing.T)
```

- [ ] **Step 2: Fail'i doğrula**

Run: `go test ./tests/conformance/contracts -run 'TestResponse|TestUsage' -v` → FAIL.

- [ ] **Step 3: Şemaları yaz**

`response-create.json` — spec §8.2 alan seti: `model`, `instructions`, `input` (string veya `content.json` array), `tools` (`[{"ref": "..."}]`), `tool_sets`, `skills`, `tool_choice` (`auto|none|required` open), `parallel_tool_calls`, `context`, `delegation` (`mode`, `max_depth`, `max_children`), `capabilities`, `output.format`, `max_output_tokens`, `max_tool_calls`, `budget` (`max_cost_usd`, `max_duration_seconds`), `store`, `background`, `stream`, `previous_response_id` (nullable), `session_id` (nullable), `workspace`, `repository`, `callback`, `metadata`, `agent_revision_id` (nullable), `engine` (nullable). Required: `["input"]`. Mutual exclusion:

```json
"allOf": [{"not": {"required": ["previous_response_id", "session_id"]}}]
```

`response.json`: `resource.json` alanlarını `$ref` ile alır; `status` open string, dokümante `$defs.known_statuses`: `queued`, `provisioning`, `in_progress`, `waiting_for_tool`, `waiting_for_approval`, `waiting_for_input`, `completed`, `failed`, `canceled`, `timed_out`, `budget_exceeded`; `object` const `"response"`; `model` string (actually-used model); `output` array of `content.json`; `usage` `$ref usage.json`; `error` `oneOf [problem.json, null]`; `session_id`/`run_id` opsiyonel referanslar.

`usage.json`: required `["input_tokens","output_tokens"]`; `input_tokens`/`output_tokens`/`total_tokens`/`tool_calls` integer ≥0; `cost` object (`amount_usd` number ≥0, `estimated` boolean); additionalProperties true.

- [ ] **Step 4: Generate + testler**

Run: `make generate && go test ./tests/conformance/contracts -run 'TestResponse|TestUsage' -v` → PASS.

- [ ] **Step 5: Commit**

```bash
git add protocols/schemas/execution protocols/generated packages/contracts tests/conformance/contracts
git commit -m "feat: define response and usage contracts"
```

### Task 5: Event envelope, event registry ve AsyncAPI 3.1

**Files:**

- Create: `protocols/schemas/execution/event.json`
- Create: `protocols/schemas/execution/event-types.json`
- Create: `protocols/asyncapi/asyncapi-3.1.yaml`
- Test: `tests/conformance/contracts/event_test.go`

**Interfaces:**

- Produces: `contracts.Event` ve registry dosyası. Task 12 property testleri her state-machine event adının bu registry'de olduğunu doğrular; E04 SSE ve E05 journal bu zarfı kullanır.

- [ ] **Step 1: Failing testleri yaz**

```go
func TestSessionEventSequenceMustBePositive(t *testing.T)
func TestUnknownEventFieldIsIgnoredAndUnknownTypeIsPreserved(t *testing.T) // API-009
func TestEventTypeNamesAreVersioned(t *testing.T) // registry'deki her ad ^[a-z0-9_]+(\.[a-z0-9_]+)*\.v[0-9]+$
func TestEventEnvelopeMatchesCloudEventsRequiredSet(t *testing.T)
```

- [ ] **Step 2: Fail'i doğrula**

Run: `go test ./tests/conformance/contracts -run TestEvent -v` → FAIL.

- [ ] **Step 3: Şemaları yaz**

`event.json` (spec §13.2, CloudEvents-compatible): required `["specversion","id","source","type","time","sequence","data"]`; `specversion` const `"1.0"`; `id` `$ref id.json#/$defs/event_id`; `type` pattern `^[a-z0-9_]+(\.[a-z0-9_]+)*\.v[0-9]+$`; `sequence` integer ≥1; `subject`, `datacontenttype` (default `application/json`), `project_id`, `session_id`, `run_id`, `attempt_id` opsiyonel; `data` object; additionalProperties true.

`event-types.json` — registry (schema değil data):

```json
{
  "registry_version": 1,
  "events": [
    "session.created.v1", "session.active.v1", "session.paused.v1", "session.closing.v1", "session.closed.v1", "session.deleted.v1",
    "message.accepted.v1",
    "response.queued.v1", "response.provisioning.v1", "response.in_progress.v1",
    "response.waiting_for_tool.v1", "response.waiting_for_approval.v1", "response.waiting_for_input.v1",
    "response.completed.v1", "response.failed.v1", "response.canceled.v1", "response.timed_out.v1", "response.budget_exceeded.v1",
    "run.queued.v1", "run.provisioning.v1", "run.running.v1", "run.waiting.v1",
    "run.completed.v1", "run.failed.v1", "run.canceled.v1", "run.timed_out.v1", "run.budget_exceeded.v1",
    "attempt.assigned.v1", "attempt.starting.v1", "attempt.active.v1", "attempt.draining.v1",
    "attempt.succeeded.v1", "attempt.failed.v1", "attempt.lost.v1", "attempt.preempted.v1",
    "command.accepted.v1", "command.applying.v1", "command.applied.v1", "command.rejected.v1", "command.expired.v1",
    "tool_call.proposed.v1", "tool_call.policy_check.v1", "tool_call.approval_pending.v1", "tool_call.ready.v1",
    "tool_call.leased.v1", "tool_call.executing.v1", "tool_call.completed.v1", "tool_call.failed.v1",
    "tool_call.canceled.v1", "tool_call.uncertain.v1", "tool_call.reconciled_completed.v1",
    "tool_call.reconciled_not_applied.v1", "tool_call.manual_resolution.v1",
    "workspace.requested.v1", "workspace.provisioning.v1", "workspace.preparing.v1", "workspace.ready.v1",
    "workspace.leased.v1", "workspace.snapshotting.v1", "workspace.paused.v1", "workspace.restoring.v1",
    "workspace.host_lost.v1", "workspace.recovering.v1", "workspace.failed.v1",
    "workspace.destroying.v1", "workspace.destroyed.v1",
    "model_step.created.v1", "model_step.delta.v1", "model_step.completed.v1", "model_step.failed.v1",
    "artifact.created.v1", "usage.updated.v1", "warning.raised.v1", "policy.denied.v1"
  ]
}
```

`asyncapi-3.1.yaml`: `channels.sessionEvents` (address `/v1/sessions/{session_id}/events`), message payload `$ref` → `event.json`, SSE binding notu; registry'deki adlar `x-event-types` olarak listelenir.

- [ ] **Step 4: Generate + testler**

Run: `make generate && go test ./tests/conformance/contracts -run TestEvent -v` → PASS.

- [ ] **Step 5: Commit**

```bash
git add protocols/schemas/execution protocols/asyncapi protocols/generated packages/contracts tests/conformance/contracts
git commit -m "feat: define the canonical event envelope and registry"
```

### Task 6: Engine ve runner protokol şemaları (ENG-001..003 schema tarafı)

**Files:**

- Create: `protocols/engine/engine.schema.json`
- Create: `protocols/runner/runner.schema.json`
- Test: `tests/conformance/contracts/engine_protocol_test.go`

**Interfaces:**

- Produces: `contracts.EngineFrame`, `contracts.RunnerMessage`. local-live-proof Task 8 bu dosyaları "Create" yerine hazır bulur; E05 supervisor bu şemayla doğrular.

- [ ] **Step 1: Failing testleri yaz**

```go
func TestEngineFrameRequiresProtocolIDTypeSequenceTime(t *testing.T)
func TestEngineFrameKnownTypesCoverSpecTables(t *testing.T) // §25.7 12 controller + §25.8 16 engine tipi registry'de
func TestEngineFrameMaxLineBytesDocumented(t *testing.T)    // $defs.limits.max_line_bytes == 1048576
func TestRunnerLeaseMessagesRequireFence(t *testing.T)      // lease.accept/renew/complete fence olmadan geçersiz
```

- [ ] **Step 2: Fail'i doğrula**

Run: `go test ./tests/conformance/contracts -run 'TestEngineFrame|TestRunnerLease' -v` → FAIL.

- [ ] **Step 3: Şemaları yaz**

`engine.schema.json` (spec §25.5–25.8): frame envelope required `["protocol","id","type","sequence","time"]`; `protocol` const `"engine.v1"`; `id` `$ref ../schemas/common/id.json#/$defs/frame_id`; `sequence` integer ≥1; `reply_to` nullable frame id; `run_id`/`attempt_id` opsiyonel; `data` object; additionalProperties true. `$defs.controller_types`: `supervisor.hello`, `run.start`, `run.restore`, `message.deliver`, `config.change`, `model.result`, `model.delta`, `tool.result`, `approval.result`, `child.result`, `checkpoint.request`, `run.pause`, `run.cancel`, `protocol.ack`. `$defs.engine_types`: `engine.ready`, `engine.heartbeat`, `progress`, `output.delta`, `output.item`, `model.request`, `tool.request`, `child.request`, `approval.request`, `checkpoint.offer`, `context.compacted`, `warning`, `protocol.error`, `run.waiting`, `run.terminal`, `protocol.ack`. `type` open string kalır (unsupported tip → `protocol.error`, reddedilmiş schema değil). `$defs.limits`: `{"max_line_bytes": 1048576}`. `engine.ready` data'sı `if/then` ile: required `["selected_protocol","engine","max_frame_bytes","nonce"]`, `engine` = `{name, version}`, `checkpoint_formats` array, `commands` array.

`runner.schema.json`: envelope required `["protocol","type","time"]`; `protocol` const `"runner.v1"`; `$defs.types`: `runner.hello`, `runner.heartbeat`, `lease.offer`, `lease.accept`, `lease.renew`, `lease.complete`, `lease.revoke`; `lease.*` mesajları `if/then` ile required `["lease_id","run_id","attempt_id"]`, `lease.accept|renew|complete` ayrıca required `["fence"]` (`integer ≥1`); `lease.offer` data'sı `image_digest` (pattern `^sha256:[a-f0-9]{64}$`) ve `limits` taşır.

- [ ] **Step 4: Generate + testler**

Run: `make generate && go test ./tests/conformance/contracts -run 'TestEngineFrame|TestRunnerLease' -v` → PASS.

- [ ] **Step 5: Commit**

```bash
git add protocols/engine protocols/runner protocols/generated packages/contracts tests/conformance/contracts
git commit -m "feat: define engine and runner protocol schemas"
```

### Task 7: OpenAPI 3.2 canonical + 3.1.2 projection + semantic diff

**Files:**

- Create: `protocols/openapi/openapi-3.2.yaml`
- Create: `scripts/contracts/semantic/` (spike'tan terfi: `spikes/contracts/semantic_check.go` + test)
- Modify: `scripts/contracts/generate` (projection adımı)
- Modify: `scripts/contracts/check` (semantic check adımı)
- Test: `tests/conformance/contracts/openapi_test.go`

**Interfaces:**

- Produces: `protocols/generated/openapi-3.1.2.yaml`; SDK generation (E07/E16) yalnızca projection'ı tüketir.

- [ ] **Step 1: Failing testleri yaz**

```go
func TestProjectionExistsAndIsRegenerated(t *testing.T)
func TestProjectionPreservesCanonicalSemantics(t *testing.T) // const/enum/nullable/required/pattern kaybı yok
func TestOpenAPICoversLp0Surface(t *testing.T) // POST /v1/responses, GET /v1/responses/{id}, POST .../cancel, GET /v1/sessions/{id}/events, GET /v1/capabilities
func TestEveryOperationDeclaresProblemErrors(t *testing.T)  // default response application/problem+json → problem.json
```

- [ ] **Step 2: Fail'i doğrula**

Run: `go test ./tests/conformance/contracts -run 'TestProjection|TestOpenAPI|TestEveryOperation' -v` → FAIL.

- [ ] **Step 3: Canonical OpenAPI'yi yaz ve projection'ı bağla**

`openapi-3.2.yaml`: `openapi: 3.2.0`; `POST /v1/responses` (header param `Idempotency-Key` required; request `$ref response-create.json`; `202` + `Location` + body `response.json`), `GET /v1/responses/{response_id}` (`200` `response.json`, `404/410` problem), `POST /v1/responses/{response_id}/cancel` (`202`), `GET /v1/sessions/{session_id}/events` (`200` `text/event-stream`, `Last-Event-ID` header param), `GET /v1/capabilities` (`200`). SecurityScheme `bearerAuth`; her response `Request-Id` ve `API-Version` header tanımı taşır. Şema gövdeleri inline kopya değil `$ref` ile `../schemas/...` dosyalarına bağlanır.

`spikes/contracts/semantic_check.go` → `scripts/contracts/semantic/` altına taşınır; `generate` projection üretir, `check` semantic check'i çağırır.

- [ ] **Step 4: Generate + testler**

Run: `make generate && make check-generated && go test ./tests/conformance/contracts -run 'TestProjection|TestOpenAPI|TestEveryOperation' -v` → PASS.

- [ ] **Step 5: Commit**

```bash
git add protocols/openapi protocols/generated scripts/contracts tests/conformance/contracts
git commit -m "feat: publish canonical OpenAPI with verified projection"
```

### Task 8: Cross-language fixture corpus ve round-trip checks

**Files:**

- Create: `protocols/fixtures/corpus/omitted.json`
- Create: `protocols/fixtures/corpus/null.json`
- Create: `protocols/fixtures/corpus/empty.json`
- Create: `protocols/fixtures/corpus/unknown-field.json`
- Create: `protocols/fixtures/corpus/unknown-enum.json`
- Create: `protocols/fixtures/corpus/rfc3339.json`
- Create: `protocols/fixtures/corpus/int-boundary.json`
- Create: `protocols/generated/typescript/corpus_check.ts` (generator şablonundan)
- Create: `protocols/generated/python/corpus_check.py` (generator şablonundan)
- Modify: `scripts/contracts/check` (üç dilli corpus adımı)
- Test: `tests/conformance/contracts/corpus_test.go`

**Interfaces:**

- Produces: master plan E02 "cross-language fixtures corpus" checkbox'ı; E16 SDK parity (API-012) aynı corpus'u yeniden kullanır.

- [ ] **Step 1: Failing corpus testini yaz**

```go
func TestCorpusRoundTripsInGo(t *testing.T) {
	// her corpus dosyası: decode → encode → byte-level semantic eşitlik
	// omitted alan null'a dönmez; null alan kaybolmaz; unknown alan/enum değeri korunur
}
func TestCorpusCoversMandatoryCases(t *testing.T) {
	// omitted, null, empty, unknown-field, unknown-enum, rfc3339, int-boundary dosyalarının yedisi de mevcut ve boş değil
}
```

Corpus dosya formatı:

```json
{
  "case": "int-boundary",
  "schema": "https://schemas.palai.dev/execution/event.json",
  "documents": [
    {"note": "zero", "value": {"specversion": "1.0", "id": "evt_a", "source": "/v1/sessions/ses_a", "type": "run.queued.v1", "time": "2026-07-16T12:00:00.000000Z", "sequence": 1, "data": {"n": 0}}},
    {"note": "int32-max", "value": {"specversion": "1.0", "id": "evt_b", "source": "/v1/sessions/ses_a", "type": "run.queued.v1", "time": "2026-07-16T12:00:00.000000Z", "sequence": 2147483647, "data": {}}},
    {"note": "int53-max", "value": {"specversion": "1.0", "id": "evt_c", "source": "/v1/sessions/ses_a", "type": "run.queued.v1", "time": "2026-07-16T12:00:00.000000Z", "sequence": 9007199254740991, "data": {}}}
  ]
}
```

`rfc3339.json` mikro-saniye, saniye ve `Z`/`+00:00` varyantlarını; `unknown-enum.json` bilinmeyen `status`/`role`/frame `type` değerlerini; `omitted.json`/`null.json` `error`, `metadata`, `previous_response_id` üçlüsünü kapsar.

- [ ] **Step 2: Fail'i doğrula**

Run: `go test ./tests/conformance/contracts -run TestCorpus -v` → FAIL.

- [ ] **Step 3: Corpus'u ve üç dilli check'i uygula**

Yedi corpus dosyası yazılır. Generator'ın spike'taki `check.ts.tmpl`/`check.py.tmpl` şablonları corpus'u okuyup round-trip eşitliğini assert eden `corpus_check.ts`/`corpus_check.py` üretir. `scripts/contracts/check` sonuna eklenir:

```bash
pnpm exec tsc --noEmit -p "$root/protocols/generated/typescript"
node --experimental-strip-types "$root/protocols/generated/typescript/corpus_check.ts"
PYTHONDONTWRITEBYTECODE=1 python3 "$root/protocols/generated/python/corpus_check.py"
```

- [ ] **Step 4: Üç dilli check'i çalıştır**

Run: `make check-generated && go test ./tests/conformance/contracts -run TestCorpus -v` → PASS; TS/Python check çıktıları `corpus=PASS` satırı üretir.

- [ ] **Step 5: Commit**

```bash
git add protocols/fixtures protocols/generated scripts/contracts tests/conformance/contracts
git commit -m "test: add the cross-language contract corpus"
```

### Task 9: state-machines çekirdeği: Apply, fence ve sequence guard + Run/Attempt tabloları

**Files:**

- Create: `packages/state-machines/statemachine.go`
- Create: `packages/state-machines/fence.go`
- Create: `packages/state-machines/sequence.go`
- Create: `packages/state-machines/run.go`
- Create: `packages/state-machines/attempt.go`
- Test: `packages/state-machines/statemachine_test.go`
- Test: `packages/state-machines/run_test.go`

**Interfaces:**

- Produces (sonraki tüm task'lar ve E03/E04/E08 bunları kullanır; imza local-live-proof Task 4 ile birebir):

```go
package statemachines // import "github.com/palgroup/palai/packages/state-machines"

type Transition[S comparable, C comparable] struct {
	From    S
	Command C
	To      S
	Event   string
}

func Apply[S comparable, C comparable](current S, command C, table []Transition[S, C]) (S, string, error)
func TerminalStates[S comparable, C comparable](table []Transition[S, C]) map[S]bool

var ErrInvalidState error        // stable code: invalid_state
var ErrStaleFence error          // stable code: lease_conflict
var ErrNonMonotonicSequence error

func AcceptFence(current, offered uint64) error
func NextSequence(prev, next int64) error
```

- [ ] **Step 1: Failing çekirdek testlerini yaz**

```go
func TestApplyReturnsTableRow(t *testing.T)
func TestApplyRejectsUnknownTransitionWithInvalidState(t *testing.T)
func TestTerminalStatesHaveNoOutgoingRows(t *testing.T)
func TestAttemptRequiresIncreasingFence(t *testing.T) // AcceptFence(5,5)/(5,4) → ErrStaleFence; (5,6) → nil
func TestNextSequenceIsStrictlyMonotonic(t *testing.T)
func TestRunTerminalityIsMonotonic(t *testing.T)      // completed'dan her komut ErrInvalidState
```

- [ ] **Step 2: Fail'i doğrula**

Run: `go test ./packages/state-machines -v` → FAIL (package yok).

- [ ] **Step 3: Çekirdeği ve Run/Attempt tablolarını uygula**

`statemachine.go`:

```go
func Apply[S comparable, C comparable](current S, command C, table []Transition[S, C]) (S, string, error) {
	for _, tr := range table {
		if tr.From == current && tr.Command == command {
			return tr.To, tr.Event, nil
		}
	}
	var zero S
	return zero, "", fmt.Errorf("%w: no transition from %v via %v", ErrInvalidState, current, command)
}

func TerminalStates[S comparable, C comparable](table []Transition[S, C]) map[S]bool {
	out := map[S]bool{}
	for _, tr := range table {
		if _, ok := out[tr.To]; !ok {
			out[tr.To] = true
		}
	}
	for _, tr := range table {
		out[tr.From] = false
	}
	return out
}
```

`run.go` (spec §22.3): states `queued, provisioning, running, waiting, completed, failed, canceled, timed_out, budget_exceeded`; commands `provision, start, wait, resume, complete, fail, cancel, timeout, exhaust_budget`. Tablo satırları:

```go
var RunTable = []Transition[RunState, RunCommand]{
	{RunQueued, RunCmdProvision, RunProvisioning, "run.provisioning.v1"},
	{RunProvisioning, RunCmdStart, RunRunning, "run.running.v1"},
	{RunRunning, RunCmdWait, RunWaiting, "run.waiting.v1"},
	{RunWaiting, RunCmdResume, RunRunning, "run.running.v1"},
	{RunRunning, RunCmdComplete, RunCompleted, "run.completed.v1"},
	{RunQueued, RunCmdCancel, RunCanceled, "run.canceled.v1"},
	{RunProvisioning, RunCmdCancel, RunCanceled, "run.canceled.v1"},
	{RunRunning, RunCmdCancel, RunCanceled, "run.canceled.v1"},
	{RunWaiting, RunCmdCancel, RunCanceled, "run.canceled.v1"},
	{RunQueued, RunCmdFail, RunFailed, "run.failed.v1"},
	{RunProvisioning, RunCmdFail, RunFailed, "run.failed.v1"},
	{RunRunning, RunCmdFail, RunFailed, "run.failed.v1"},
	{RunWaiting, RunCmdFail, RunFailed, "run.failed.v1"},
	{RunRunning, RunCmdTimeout, RunTimedOut, "run.timed_out.v1"},
	{RunWaiting, RunCmdTimeout, RunTimedOut, "run.timed_out.v1"},
	{RunRunning, RunCmdExhaustBudget, RunBudgetExceeded, "run.budget_exceeded.v1"},
	{RunWaiting, RunCmdExhaustBudget, RunBudgetExceeded, "run.budget_exceeded.v1"},
}
```

`attempt.go` (spec §22.3): states `assigned, starting, active, draining, succeeded, failed, lost, preempted`; commands `start, activate, drain, succeed, fail, lose, preempt`; `fail/lose/preempt` `assigned|starting|active|draining` durumlarının hepsinden satırla tanımlanır; `succeed` yalnızca `draining`den (`attempt.succeeded.v1`).

- [ ] **Step 4: Testleri çalıştır**

Run: `go test ./packages/state-machines -v` → PASS.

- [ ] **Step 5: Commit**

```bash
git add packages/state-machines
git commit -m "feat: add the state-machine core with run and attempt tables"
```

### Task 10: Response ve Session tabloları

**Files:**

- Create: `packages/state-machines/response.go`
- Create: `packages/state-machines/session.go`
- Test: `packages/state-machines/response_test.go`
- Test: `packages/state-machines/session_test.go`

**Interfaces:**

- Consumes: Task 9 `Apply`/`Transition`.
- Produces: `ResponseTable` (§8.3 lifecycle: `waiting_for_tool/approval/input` ↔ `in_progress` döngüleri dahil), `SessionTable` (§22.1: `active ↔ paused`, `active|paused → closing → closed → deleted`).

- [ ] **Step 1: Failing testleri yaz**

```go
func TestResponseWaitingStatesReturnOnlyToInProgress(t *testing.T)
func TestResponseTerminalsAreMonotonic(t *testing.T) // 5 terminal durumdan hiçbir komut kabul edilmez
func TestSessionTerminalRunDoesNotCloseSession(t *testing.T) // session tablosunda run kaynaklı transition yoktur
func TestSessionDeleteRequiresClosed(t *testing.T)
```

- [ ] **Step 2: Fail'i doğrula**

Run: `go test ./packages/state-machines -run 'TestResponse|TestSession' -v` → FAIL.

- [ ] **Step 3: Tabloları uygula**

`response.go` commands: `provision, start, request_tool, request_approval, request_input, resume, complete, fail, cancel, timeout, exhaust_budget`. `in_progress → waiting_for_*` üç komutla, her `waiting_for_* → in_progress` `resume` ile döner (`response.in_progress.v1`); terminale geçişler `in_progress` ve üç `waiting_*` durumundan tanımlanır; `cancel` `queued/provisioning`dan da geçerlidir. Event adları registry'deki `response.*` adlarıdır.

`session.go` commands: `pause, resume, close, finish_close, delete`. Satırlar: `active→paused (pause)`, `paused→active (resume)`, `active→closing (close)`, `paused→closing (close)`, `closing→closed (finish_close)`, `closed→deleted (delete)`. Session `active` doğar; `created` bir state değildir (spec §22.1).

- [ ] **Step 4: Testleri çalıştır**

Run: `go test ./packages/state-machines -run 'TestResponse|TestSession' -v` → PASS.

- [ ] **Step 5: Commit**

```bash
git add packages/state-machines
git commit -m "feat: add response and session state tables"
```

### Task 11: Command, ToolCall ve Workspace tabloları

**Files:**

- Create: `packages/state-machines/command.go`
- Create: `packages/state-machines/tool_call.go`
- Create: `packages/state-machines/workspace.go`
- Test: `packages/state-machines/tool_call_test.go`
- Test: `packages/state-machines/workspace_test.go`

**Interfaces:**

- Produces: `CommandTable`, `ToolCallTable`, `WorkspaceTable`; E03 coordinator ve E05 supervisor bu tabloları transaction içinde çağırır.

- [ ] **Step 1: Failing testleri yaz**

```go
func TestToolCallUncertainCannotBecomeCompletedWithoutReconciliation(t *testing.T)
// uncertain'dan complete komutu ErrInvalidState; yalnızca reconcile_completed/reconcile_not_applied/escalate geçerli
func TestToolCallOnlyCompletedFamiliesAreSuccessful(t *testing.T)
// SuccessfulToolStates() == {completed, reconciled_completed} (spec §26.7)
func TestWorkspaceLeaseCyclesThroughReadyAndSnapshotting(t *testing.T)
func TestWorkspaceDestroyAllowedOnlyFromReadyPausedFailed(t *testing.T)
func TestCommandDuplicateApplicationIsInvalid(t *testing.T) // applied'dan apply komutu ErrInvalidState
```

- [ ] **Step 2: Fail'i doğrula**

Run: `go test ./packages/state-machines -run 'TestToolCall|TestWorkspace|TestCommand' -v` → FAIL.

- [ ] **Step 3: Tabloları uygula**

`tool_call.go` (spec §26.7): states `proposed, policy_check, approval_pending, ready, leased, executing, completed, failed, canceled, uncertain, reconciled_completed, reconciled_not_applied, manual_resolution`; commands `check_policy, require_approval, approve, mark_ready, lease, execute, complete, fail, cancel, mark_uncertain, reconcile_completed, reconcile_not_applied, escalate`. Akış: `proposed→policy_check→(approval_pending→)ready→leased→executing`; `executing→completed|failed|canceled|uncertain`; `uncertain→reconciled_completed|reconciled_not_applied|manual_resolution`; `cancel` `proposed..executing` tüm non-terminal durumlarından geçerli. Yardımcı: `func SuccessfulToolStates() map[ToolCallState]bool`.

`workspace.go` (spec §29.7): states `requested, provisioning, preparing, ready, leased, snapshotting, paused, restoring, host_lost, recovering, failed, destroying, destroyed`; commands `provision, prepare, mark_ready, lease, release, snapshot, finish_snapshot, pause, restore, lose_host, recover, fail, destroy, finish_destroy`. Satırlar diyagramı izler: `ready↔leased`, `ready↔snapshotting`, `preparing|ready→paused→restoring→ready`, `leased→host_lost→recovering→ready|failed`, `ready|paused|failed→destroying→destroyed`.

`command.go`: states `queued, applying, applied, rejected, expired`; commands `apply, finish_apply, reject, expire`. Not: spec §22.4 command state'lerini enumerate etmez; bu tablo acceptance/`applied_sequence`/expiry dilinden türetilen minimum settir ve genişleme additive olmak zorundadır.

- [ ] **Step 4: Testleri çalıştır**

Run: `go test ./packages/state-machines -run 'TestToolCall|TestWorkspace|TestCommand' -v` → PASS.

- [ ] **Step 5: Commit**

```bash
git add packages/state-machines
git commit -m "feat: add command, tool-call, and workspace state tables"
```

### Task 12: Property suite, registry cross-check ve E02 exit gate

**Files:**

- Create: `packages/state-machines/property_test.go`
- Create: `packages/state-machines/registry_test.go`
- Modify: `Makefile` (`test-unit` kapsamı `./packages/... ./tests/conformance/...` içermiyorsa genişletilir)
- Modify: `docs/superpowers/plans/2026-07-16-self-hosted-master-plan.md` (E02 checkbox'ları)
- Modify: `docs/superpowers/plans/2026-07-16-local-live-proof.md` (Task 2/4 karşılandı notu)

**Interfaces:**

- Produces: E02 exit gate kanıtı — zero drift, tüm property'ler yeşil, handler yok.

- [ ] **Step 1: Failing property testlerini yaz**

Yedi tabloyu tek string-facade registry'de topla:

```go
type tableSpec struct {
	name     string
	initial  string
	commands []string
	apply    func(state, command string) (string, string, error)
	terminal map[string]bool
}

func allTables() []tableSpec // run, attempt, response, session, command, tool_call, workspace
```

Property'ler:

```go
func TestEveryTransitionPairIsUniqueAndEmitsExactlyOneEvent(t *testing.T)
// (From,Command) tekildir; Event boş değildir

func TestTerminalMonotonicityUnderRandomCommandSequences(t *testing.T)
// seed'li rand ile 10k rastgele komut dizisi: terminale girince sonraki her komut ErrInvalidState

func TestOneActiveFenceUnderRandomInterleavings(t *testing.T)
// rastgele fence teklif dizisi: yalnızca strictly-increasing kabul; kabul edilen dizi sorted+unique

func TestSequenceGuardIsStrictlyMonotonic(t *testing.T)

func TestEveryTableEventExistsInRegistry(t *testing.T)
// registry_test.go: protocols/schemas/execution/event-types.json okunur;
// tüm tabloların Event sütunu registry'nin alt kümesidir
```

- [ ] **Step 2: Fail'i doğrula**

Run: `go test ./packages/state-machines -run 'TestEvery|TestTerminal|TestOneActive|TestSequenceGuard' -v`

Expected: `allTables`/facade tanımsız → FAIL.

- [ ] **Step 3: Facade'ı uygula ve suite'i geçir**

Her tablo için tip-güvenli küçük adapter (string→typed sabit map'i) yazılır; production API değişmez, facade `_test.go` içinde kalır.

- [ ] **Step 4: Tam doğrulama**

Run: `go test ./packages/state-machines -count=100 && make generate && make check-generated && make verify`

Expected: hepsi PASS; `git status --short` yalnızca beklenen değişiklikleri gösterir; `apps/` altında yeni handler dosyası yoktur.

- [ ] **Step 5: Plan checkbox'larını işle ve commit'le**

Master planda E02'nin altı checkbox'ı işaretlenir; local-live-proof Task 2/4'e "phase-02 Task 1–12 ile karşılandı (commit <sha>)" notu düşülür.

```bash
git add packages/state-machines Makefile docs/superpowers/plans
git commit -m "test: prove contract spine invariants"
```

## Final E02 exit gate

- [ ] `make generate && make check-generated` zero diff (iki kez üst üste).
- [ ] `go test ./packages/state-machines -count=100` PASS.
- [ ] `go test ./tests/conformance/contracts/...` PASS (Go + TS + Python corpus dahil).
- [ ] `make verify` PASS.
- [ ] `apps/control-plane` altında business logic/handler eklenmemiş (yalnızca E04'te başlar).
- [ ] Master plan E02 ve local-live-proof Task 2/4 kayıtları güncel.

## UAT ownership

| UAT | Bu plandaki kanıt |
|---|---|
| API-009 (unknown fields/enums) | Task 3/5/8 preserve testleri + corpus |
| API-011 (RFC 9457) | Task 2 problem şeması + known_codes testi (runtime davranışı E04'te) |
| ENG-001..003 schema tarafı | Task 6 handshake/frame/limit şemaları (runtime davranışı E05'te) |
