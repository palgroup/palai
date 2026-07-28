# Where Palai runs, what it is, and what stands between here and a stranger installing it

**Date:** 2026-07-28 · **Author:** research pass against `ed44544` (main, clean) · **Method:** repo first, web second. Every external claim carries a URL and the date it was fetched. Prices carry dates because prices move. Anything I could not verify is labelled **unverified** rather than estimated.

**Companion documents:** [`macos-isolation-without-accounts.md`](./macos-isolation-without-accounts.md) (2026-07-28) and [`macos-session-isolation.md`](./macos-session-isolation.md) (2026-07-26) settle the Mac isolation question; this document takes their verdict as given and asks what it costs. [`../operations/support-matrix.json`](../operations/support-matrix.json) and [`../operations/known-gaps-1.0.md`](../operations/known-gaps-1.0.md) are the authoritative statements of what is proven; this document does not restate them, it ranks what they imply.

---

## 1. THE VERDICT — one page

**The thing blocking adoption is not the amd64 gap, and it is not the admin panel. It is that Palai is not published anywhere.** `@palai/sdk` is `"private": true` and returns 404 on npm; `palai/control-plane` returns 404 on Docker Hub; there is no PyPI package; the release workflow's publish step is literally named *"Publish mechanics - DRY RUN (no credentials, nothing reaches a registry)"* (`.github/workflows/release.yml:207`). The `palai` CLI resolves its compose file as `deploy/compose/compose.yaml` **relative to the current directory** (`cmd/cli/internal/stack/config.go:57–62`), and `docs/operations/install.md` tells the operator to `go build -o palai ./cmd/cli` and bring the stack up with `--build`. **The only way anyone can run Palai today is to clone the repository and compile it.** That is not a self-hostable product; it is a repository that happens to contain one. Everything else on this list is a second-order problem.

**The amd64 gap is smaller than it looks and worse than it looks, in different ways.** Smaller: I checked, and there is nothing architecture-specific in the tree. Zero `import "C"`, every build is `CGO_ENABLED=0`, the OCI sandbox driver (`adapters/sandboxes/oci/`) contains no architecture reference at all, and every one of the five third-party digest pins — postgres, seaweedfs, caddy, python, golang, alpine — resolves to a **multi-architecture index that includes `linux/amd64`** (I queried the registry directly; see §2.3). CI already runs `make verify` on `ubuntu-24.04`, which is amd64, on every push. So the code is portable and the unit/component tiers already run on amd64 every day. Worse: the *cells that say `not-tested`* are the ones that matter to a buyer — production compose, Kubernetes, air-gap — and those are the journey drills that need Docker, `kind` and a running stack. They were only ever run on the owner's arm64 Mac. **This is a CI-runner problem, not a portability problem**, and `known-gaps-1.0.md` already books it as `L-3` ("a **full UAT run on amd64**", operator, E18 §6 leg 3). Turning those cells green is buying an amd64 runner and letting the existing `make uat-*` targets run — days, not weeks. And it needs doing, because **roughly 91% of CPUs in tracked Kubernetes clusters are still x86** ([Cast AI, 2026-07-22](https://cast.ai/blog/kubernetes-arm-graviton-nodes/), fetched 2026-07-28). Shipping an infrastructure product whose only tested column covers 9% of the market is the kind of thing a serious evaluator finds in ten minutes and never mentions to you.

**The admin panel is real but is not the gate.** The console has exactly two pages (`app/page.tsx`, `app/runs/page.tsx`). The `palai` CLI covers exactly four configuration surfaces — `org`, `project`, `apikey`, `secret` (`cmd/cli/internal/admin/admin.go`). Everything else — agents and their revisions, tools and tool sets, **model connections and model routes**, MCP connections, Slack connections, knowledge bases, skills, hooks, triggers, schedules, webhooks, budgets, quotas, repository bindings, queue connections — is raw HTTP against `/v1/*`. The single most damning item on that list is **model routes**: a fresh install cannot execute one run until someone POSTs a model connection and a model route and publishes a revision, and there is no UI and no CLI for any of it. First-run is `curl`. But note the ordering: a stranger who cannot obtain the software never reaches the point of complaining about `curl`.

**The Mac problem has a cheap honest answer, and Palai does not currently ship the piece that would use it.** The isolation research is settled — different customers must get different Macs. Good news: a dedicated Mac mini M4 is **€149/month** at Scaleway or **$149/month** at MacStadium (both fetched 2026-07-28), which is a normal per-customer cost of goods, not an exotic one. Bad news: EC2 Mac is roughly **$898/month** for the same class of machine (`mac-m4.metal` at $1.23/hour × 730), six times the price, and its **24-hour minimum host allocation** means a one-hour iOS build costs you 24 hours of billing — $29.52 on AWS, €5.28 on Scaleway (which has the same 24-hour floor). Worse news, and this is the part that is a repo fact rather than a pricing fact: **`scripts/release/build.sh` does not build `palai-capability-worker` at all.** The macOS side of the flagship use case — the worker that would run on that Mac — is not in the release matrix, is not in the support matrix, and `apple-build` is recorded as `disabled` in every shipped evidence bundle because no Apple signing material exists anywhere in the program.

**So the honest shape of the product today:** a well-evidenced, genuinely well-tested Linux control plane with a 1.0 RC that has zero blockers *by its own gate's definition* — and that gate is honest, I checked both guards (`TestKnownGapsRCBlockerCountIsAccurate` and `TestSupportMatrixOnlyClaimsWhatIsProven` both exist in `tests/docs/ops_docs_test.go`) — that **nobody outside this machine can install, on the architecture 91% of the market uses, configured through an API with no UI, with the iOS half of the story not built into any release artifact.** Zero RC blockers is a true statement about a release gate. It is not a statement about shippability, and the `L-1`…`L-8d` operator-leg table at the bottom of `known-gaps-1.0.md` is where the real list has been sitting all along.

---

## 2. Question 1 — the amd64 gap: how bad is it, really?

### 2.1 What the market actually runs

| Signal | Figure | Source | Fetched |
|---|---|---|---|
| ARM share of CPUs in tracked Kubernetes clusters | **~9%** (so x86 ≈ 91%) | Cast AI, *2026 State of Kubernetes Optimization Report*, via [cast.ai blog, published 2026-07-22](https://cast.ai/blog/kubernetes-arm-graviton-nodes/) | 2026-07-28 |
| ARM node growth rate | 3.5× faster than x86, Q2 2024 → Q4 2025 | same | 2026-07-28 |
| GKE Arm | Axion (C4A) and Tau T2A node pools; GKE auto-taints ARM pools `kubernetes.io/arch=arm64:NoSchedule` | [Arm Learning Paths, GKE multi-arch Axion](https://learn.arm.com/learning-paths/servers-and-cloud-computing/gke-multi-arch-axion/) | 2026-07-28 |
| EKS Arm | Graviton managed node groups since 2020; **no automatic taint**, explicit NodePool config required | [EKS Workshop, Graviton](https://www.eksworkshop.com/docs/fundamentals/compute/managed-node-groups/graviton/) | 2026-07-28 |
| AKS Arm | Azure Cobalt exists; AKS node-pool detail **unverified** — I could not find an authoritative current statement | — | 2026-07-28 |
| Hetzner Cloud | CAX (Ampere Altra, arm64) €5.99–€40.99/mo; CX (x86) €5.49–€29.49; CPX (AMD EPYC) €19.49–€129.99; CCX (dedicated) €42.99–€853.49 | [costgoat Hetzner pricing](https://costgoat.com/pricing/hetzner), pricing dated 2026-06-29 reflecting the 2026-06-15 increase | 2026-07-28 |
| Hetzner CAX geography | **Conflicting sources.** One says Falkenstein/Nuremberg/Helsinki **+ Ashburn**; another says EU-only. Hillsboro is x86-only in both. | [Better Stack review](https://betterstack.com/community/guides/web-servers/hetzner-cloud-review/), [Hetzner locations docs](https://docs.hetzner.com/cloud/general/locations/) | 2026-07-28 — **treat as unverified** |
| AWS positioning | Graviton marketed as the default recommendation; up to 40% better price-performance, ~20% lower cost than comparable x86 | [aws.amazon.com/ec2/graviton](https://aws.amazon.com/ec2/graviton/) | 2026-07-28 |
| DigitalOcean / Azure / GCP default droplet & VM architecture for a first-time user | **unverified** — not established in this pass | — | — |

**Read of it.** Arm is winning on price-performance and is growing fast, and Hetzner's June 2026 increase widened the gap further (CAX rose ~30–38% while CPX rose ~107–204%, per the same sources). But growing fast from 9% still leaves **nine out of ten clusters on x86**. A buyer evaluating an infrastructure product does not pick their architecture to suit your test matrix; they have an existing estate. "Tested on the architecture you don't run" reads, to that buyer, as untested.

### 2.2 Is "arm64 tested, amd64 built-but-untested" a defensible posture?

**As a posture, no. As a documented statement, it is unusually good.** The distinction matters and the repo is on the right side of it.

Most vendors in this space do not publish a support matrix at all, let alone a machine-guarded one where `TestSupportMatrixOnlyClaimsWhatIsProven` fails the build if a `tested` cell cites a proof id that does not resolve. That is genuinely better practice than the comparables in §3. The problem is not honesty — it is that the honest answer is bad. An evaluator who opens `support-matrix.md` learns, correctly, that production compose, Kubernetes and air-gap have never been run on the architecture they use. Having told them the truth does not make them more likely to proceed.

There is also a second-order effect worth naming: because the matrix is guarded, **the gap cannot be papered over**. Nobody can quietly flip a cell. That is a feature, and it means the only way to fix the optics is to actually fix the fact.

### 2.3 What would it cost to turn those cells green? I checked whether it is portability work. It is not.

I ran the checks rather than assuming:

| Check | Result |
|---|---|
| `import "C"` anywhere in the tree | **none** |
| Every Go build | `CGO_ENABLED=0` — in `scripts/release/build.sh`, `scripts/release/airgap-build.sh`, `scripts/package/runner/build.sh`, `scripts/test/runner-engine.sh` |
| Architecture references in the OCI sandbox driver (`adapters/sandboxes/oci/`) | **zero** — no `amd64`, `arm64`, `x86_64`, `aarch64` or platform string in any non-test file |
| `runtime.GOARCH` in production code | only two places, both cosmetic: the capability worker's enrolment banner and the performance-harness profile stamp |
| Dockerfile base images | `golang:1.26.4` and `alpine:3.21` (control-plane + runner), `python:3.14.3-slim` (reference engine) — all digest-pinned, all cross-compiled via `FROM --platform=$BUILDPLATFORM` so no qemu is involved |
| Do the digest pins resolve on amd64? | **Yes, all of them.** I queried the Docker registry directly for each pinned digest. `postgres`, `caddy` and `python` are OCI image indexes listing `linux/amd64` among 7–8 platforms; `chrislusf/seaweedfs` is a Docker manifest list with `linux/amd64, linux/arm64, linux/arm, linux/386`. A digest pin to a *single-arch manifest* would have broken amd64 outright — none of them do. |
| Where does CI run? | `ubuntu-24.04` on all three jobs in `ci.yml` and `release.yml` — **amd64**. `make verify` (lint, generated-check, unit, spikes, boundary, foundation, escape-suite Docker-free half) already runs on amd64 on every push. |
| Release matrix | `scripts/release/build.sh` defaults to `linux/amd64,linux/arm64` images, `darwin/amd64,darwin/arm64,linux/amd64,linux/arm64` CLI, `amd64,arm64` runner packages. amd64 artifacts are already built and digest-indexed. |

**Conclusion: it is CI runners, not portability work.** What is missing is that the *journey* tiers — `make uat-self-host`, `make uat-sh2`, `make uat-kind`, the air-gap drill, the DR drills — need Docker, `kind`, and a live stack, and they have only ever been executed on the owner's arm64 Mac (the matrix says so explicitly: *"Every runbook transcript in `docs/operations/runbooks/transcripts` was recorded here"*).

Cost estimate, **flagged as an estimate**: an amd64 Linux box with Docker — a Hetzner CX43 at €15.99/month or a GitHub-hosted `ubuntu-24.04` runner you already have — plus the work of making the UAT scripts runnable non-interactively in that environment and recording a second set of transcripts. The existing gate machinery (`caseChecksumParts`, the bundle sweep, the proof-id resolver) will resist fabricated evidence, which is correct and also means this cannot be faked; the runs have to actually happen. I would size it as **one focused task, not an epic** — but I have not attempted it and that number is unverified.

The one thing that genuinely cannot be closed this way is the honest ceiling the matrix already names: kind's `kindnet` does not enforce NetworkPolicy, so default-deny is proven as a render. That needs a real cluster with an enforcing CNI, on either architecture. It is booked as `L-8b`.

---

## 3. Question 2 — what does "deploy it to your own cloud" mean in 2026?

### 3.1 What comparable products actually ship

All fetched 2026-07-28.

| Product | Docker images | Compose | Helm | Terraform | Marketplace / one-click | Notes |
|---|---|---|---|---|---|---|
| **Temporal** | `temporalio/server`, `temporalio/ui-server` on a public registry | yes, in `samples-server` | yes, `temporalio/helm-charts` | community (ECS/Terraform blog posts) | not found | Docs are explicit that the image *"is what you need to use in your production environments"*; the Helm chart carries a version-compatibility caveat. ([docs.temporal.io/self-hosted-guide/deployment](https://docs.temporal.io/self-hosted-guide/deployment)) |
| **Langfuse** | `langfuse/langfuse`, `langfuse/worker` | yes, for testing/low-scale — docs say it lacks HA, scaling and backup | yes, `langfuse/langfuse-k8s`, recommended for production | **official AWS, Azure and GCP Terraform modules** | not found | The clearest "here is the production path" of the set. ([langfuse.com/self-hosting](https://langfuse.com/self-hosting)) |
| **n8n** | public images | yes, the mainstream path | yes | — | **AWS Marketplace AMI** (Ubuntu 24.04, Docker Compose on first boot, systemd, persistent storage); Coolify and Dokploy one-click templates | ([AWS Marketplace listing](https://aws.amazon.com/marketplace/pp/prodview-jrt2qhw5rbe72), [Northflank guide](https://northflank.com/blog/how-to-self-host-n8n-setup-architecture-and-pricing-guide)) |
| **Dify** | public images | yes | community Helm charts | **`langgenius/dify-ee-terraform-aws`** for the enterprise edition; AWS CDK reference in `aws-samples/dify-self-hosted-on-aws` (~20 min deploy) | not found | ([github.com/langgenius/dify](https://github.com/langgenius/dify)) |
| **Airbyte** | public images | yes | yes, `airbyte/airbyte` on Artifact Hub — v2 chart is now mandatory | community | not found | ([artifacthub.io/packages/helm/airbyte/airbyte](https://artifacthub.io/packages/helm/airbyte/airbyte)) |
| **Supabase** | public images | yes, official | **community only** (`supabase-community/supabase-kubernetes`) — no official chart | community | not found | Notable: even Supabase does not ship an official Helm chart. ([supascale.app guide](https://www.supascale.app/blog/deploying-supabase-on-kubernetes-a-complete-helm-chart-guide)) |

### 3.2 The minimum bar a serious buyer expects

Reading across the six, the bar is not "everything". It is a short, specific list, and every one of them clears it:

1. **A published image with a version tag**, pullable without a source checkout. Every single one of them. This is the floor, and it is not negotiable — it is what "self-hostable" *means*.
2. **A compose file that works on one VM**, explicitly labelled as the try-it/low-scale path.
3. **A Helm chart**, explicitly labelled as the production path — though note that Supabase gets away with community-only, so this is a strong norm rather than an absolute.
4. **A written statement of which path is production-supported and which is not.** Langfuse's is the model: *docker-compose lacks HA, scaling and backup; use Kubernetes*.
5. **Upgrade and backup documentation.**

Terraform modules and marketplace listings are **differentiators, not entry requirements** — only Langfuse ships first-party Terraform, only n8n has a marketplace AMI. AWS Marketplace itself is not a large lift if you want it: self-service listing for AMI and container products exists, automated AMI validation takes 15–20 minutes, and AWS quotes 7–10 business days to publish, with 2–4 calendar weeks recommended end to end ([AWS Marketplace docs](https://docs.aws.amazon.com/marketplace/latest/userguide/ami-getting-started.html), [suger.io guide](https://www.suger.io/resources/guides/aws-marketplace/), both fetched 2026-07-28).

### 3.3 Where Palai sits against that bar

| Bar item | Palai today | |
|---|---|---|
| Published image | **`palai/control-plane` → HTTP 404 on Docker Hub.** Release publish is a dry run. | ❌ **fails the floor** |
| Compose file, one VM | `deploy/compose/compose.yaml` + `production.yml` with a Caddy TLS edge, digest-pinned, with a `production_guard_test.go` asserting the posture. Genuinely good. But `compose.yaml` uses `build: context: ../..` and the documented bring-up is `--build`. | ⚠️ exists, requires the source tree |
| Helm chart | `deploy/helm/palai` — deployment, service, ingress, **networkpolicy**, **rbac** (with a `TestNoClusterRole` guard), pdb, migration Job, serviceaccount, NOTES.txt, README. This is a better chart than most of the comparables ship. | ✅ **but points at unpublished images** |
| Production-path statement | `docs/operations/support-matrix.md`, machine-guarded. Better than anyone in §3.1. | ✅ **best in class** |
| Upgrade + backup docs | `upgrade.md`, `backup-restore.md`, `dr-drills.md`, four runbooks with recorded transcripts, `palai backup/restore/verify`. | ✅ **best in class** |
| Terraform module | none | — (differentiator) |
| Marketplace listing | none | — (differentiator) |
| Air-gap bundle | `deploy/airgap` with signed offline install and tamper rejection. Nobody in §3.1 has this. | ✅ **above the bar** |

**The shape is unmistakable.** Palai is *above* the bar on the hard, unglamorous things — evidence discipline, air-gap, restricted RBAC, DR drills, honest documentation — and *below the floor* on the one easy thing everyone else does on day one. It has a better Helm chart than Supabase and a better support matrix than all six, and you cannot `docker pull` it.

---

## 4. Question 3 — the Mac problem, end to end

### 4.1 The constraint, taken as given

From [`macos-isolation-without-accounts.md`](./macos-isolation-without-accounts.md) (2026-07-28), verdict §1: there is no way to isolate concurrent agent sessions on one Mac without per-session uids, because `xcrun simctl spawn` creates processes as children of `launchd_sim` → `launchd`, severing the agent's process tree and thereby escaping any inherited sandbox — demonstrated against Apple's supported App Sandbox and against TCC. **Different customers → different Macs**, or build per-uid account machinery. Same-customer sessions are an accident-not-attacker threat model and `simctl --set` plus per-session directories handles them.

I take that as settled and do not re-derive it.

### 4.2 Mac compute options and prices

All fetched **2026-07-28**. EUR/USD conversion deliberately not applied — the FX rate is unverified and the currencies are quoted as the vendors quote them.

| Provider | Machine | Price | Minimum allocation | Source |
|---|---|---|---|---|
| **AWS EC2 Mac** | `mac-m4.metal` (10 vCPU, 24 GiB) | **$1.23/hour ≈ $898/month** (×730 h) | **24 hours** (Apple macOS SLA); billed as a Dedicated Host | [Vantage](https://instances.vantage.sh/aws/ec2/mac-m4.metal?currency=USD), [AWS Dedicated Host billing](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/dedicated-hosts-billing.html) |
| | `mac-m4pro.metal` (14 vCPU, 48 GiB) | $1.97/hour ≈ $1,438/month | 24 hours | [Vantage](https://instances.vantage.sh/aws/ec2/mac-m4pro.metal?currency=USD) |
| | `mac-m4max.metal` (16 vCPU, 128 GiB) | $6.25/hour ≈ $4,563/month | 24 hours | [Vantage](https://instances.vantage.sh/aws/ec2/mac-m4max.metal) |
| | `mac2-m2.metal` (8 vCPU, 24 GiB) | $0.65/hour ≈ $475/month | 24 hours | [Vantage](https://instances.vantage.sh/aws/ec2/mac2-m2.metal) |
| **Scaleway** | Mac mini M4-S (16 GB / 256 GB) | **€0.22/hour or €149/month** | **24 hours** (licence constraint, stated in their docs) | [scaleway.com/en/pricing/apple-silicon](https://www.scaleway.com/en/pricing/apple-silicon/) |
| | Mac mini M4-M (32 GB / 1.02 TB) | €0.29/hour or €199/month | 24 hours | same |
| | Mac mini M4 Pro (64 GB / 2.05 TB) | €0.49/hour or €335/month | 24 hours | same |
| | Mac mini M2 (16 GB / 256 GB) | €0.17/hour or €115/month | 24 hours | same |
| | Mac mini M1 (8 GB / 256 GB) | €0.11/hour or €75/month | 24 hours | same |
| **MacStadium** | M4.S (M4 10-core, 16 GB, 256 GB) | **$149/month** | monthly; **M4 volume needs an annual contract, qty 3+** | [macstadium.com/pricing](https://macstadium.com/pricing) |
| | M4.M (24 GB, 512 GB) | $249/month | as above | same |
| | M4.L (M4 Pro 12-core, 48 GB, 1 TB) | $349/month | as above | same |
| | M2.S (M2, 8 GB, 256 GB) | $109/month | monthly | same |
| **My Remote Mac** | Mac mini M4 (16 GB, 256 GB) | **from $85/month** | not stated | [myremotemac.com/pricing](https://myremotemac.com/pricing) |
| | M4 Pro | from $229/month | not stated | same |
| **Hetzner** | — | **no Apple hardware offering** | — | — |
| **Self-racked** | Mac mini M4 + colocation | colo **$50–$300 per 1U/month** in the US, e.g. Colocation America at $75/1U; a UK provider quotes £45+VAT/month for an M4 in 1.3U. Mac mini M4 hardware list price **unverified in this pass**. | capex + contract | [brightlio colocation pricing](https://brightlio.com/colocation-pricing/), [HA Hosting](https://hahosting.com/mac-mini-colocation/) |

### 4.3 The 24-hour minimum, and what it does to unit economics

This is the number that decides the architecture, so it is worth stating plainly. Both AWS and Scaleway impose a **24-hour minimum allocation** because of Apple's macOS licence terms. Per-session bursting therefore costs a full day regardless of how long the session ran:

| | Cost of one allocation (24 h minimum) | Dedicated month | **Break-even: sessions/month above which dedicated is cheaper** |
|---|---|---|---|
| AWS `mac-m4.metal` | **$29.52** | $898 | ~30 |
| Scaleway M4-S | **€5.28** | €149 | ~28 |
| MacStadium M4.S | n/a (no hourly) | $149 | — always dedicated |
| My Remote Mac M4 | n/a | $85 | — always dedicated |

A one-hour iOS build-and-PR run costs **$29.52 on EC2 Mac** and **€5.28 on Scaleway**. Those are different currencies and I have deliberately not applied an FX rate, so read the ratio as **roughly 5×** rather than exactly 5.59× — but it is the single most consequential price fact in this document, and no plausible FX rate changes the conclusion.

Two consequences:

- **EC2 Mac is the wrong default.** Roughly six times the monthly rate ($898 vs €149, same FX caveat) and roughly five times the per-burst cost. It is defensible only if a customer's compliance posture requires everything inside their AWS account — and in that case it is *their* bill, not yours, which is a Phase-1 self-host question, not a Phase-2 SaaS question.
- **Bursting barely beats dedicating.** At ~28–30 session-days a month the curves cross, and a customer doing meaningful iOS work will cross that in the first fortnight. Combined with the isolation constraint (different customers → different Macs), **per-customer dedicated Macs is not the expensive fallback — it is the correct answer**, and the "clever" bursting architecture would buy you almost nothing while adding an allocation state machine, a 24-hour billing-debt tracker and a cold-start problem.

### 4.4 The cheapest honest architecture

**Control plane central, one dedicated Mac per customer.**

- **Control plane**: the existing Linux stack, one small VM or a small managed cluster. Hetzner CAX31 (8 vCPU / 16 GB, arm64) at €20.99/month or CX43 (8 vCPU / 16 GB, x86) at €15.99/month is genuinely enough to start; scale is a later problem.
- **Mac fleet**: one Mac mini M4 per customer, at **€149/month (Scaleway)**, **$149/month (MacStadium)** or **$85/month (My Remote Mac)**. Scaleway has the advantage of being an actual cloud with an API; MacStadium has Orka if you later want virtualisation (pricing is sales-contact-only, **unverified**).
- **Wire**: the Mac runs the capability worker, which is **outbound-enrolled** — it dials the control plane, the control plane never dials it. That is the right shape for machines behind other people's NAT, and it already exists in the tree (`internal/workers`, `cmd/palai-capability-worker`).
- **Cost of doing nothing clever**: control plane €21 + one Mac €149 = **~€170/month per customer**, all-in, before model spend. For a product whose output is merged iOS pull requests, that is not a scary number.

**The gap between that architecture and the repo:**

1. **`palai-capability-worker` is not in the release matrix.** `scripts/release/build.sh` has zero references to it. There is no darwin binary of the worker in any release artifact.
2. **The support matrix has no row for it.** Its `surfaces` list is container-images, local-compose, production-compose, kubernetes-helm, air-gap, runner-host-package, cli. No Mac worker. Every `darwin/*` server cell is `not-applicable` because *"Palai's services ship only as linux images"* — which is true, and which means the Mac story is currently outside the matrix entirely rather than inside it as `not-tested`.
3. **`apple-build` is `disabled`** in every shipped evidence bundle (`extensions-0.1.0`, `integration-wiring-0.1.0`, `tools-memory-0.1.0`, `slack-agent-surface-0.1.0`) with the stated reason *"no vector store and no Apple signing material exist anywhere"*. `L-8d` books "real Xcode + Apple signing" as an operator leg.
4. **`internal/workers` has no production importer in the control plane.** `docs/superpowers/plans/phase-19-integration-wiring.md:128` records this precisely: the worker gateway is tested but `palai-control-plane/main.go` does not import it; the only production importer is the worker binary itself, i.e. the client side. So `capability-workers: stable` is currently advertised by a binary that never stands the gateway up.

None of these are hard, but together they mean **the Mac half of the flagship use case has infrastructure and no delivery**. Someone would have to build the worker for darwin, ship it, wire the gateway into the control plane's main, and obtain an Apple Developer account.

---

## 5. Question 4 — the admin panel: what is the minimum?

### 5.1 The configuration surface, enumerated from `apps/control-plane/api/router.go`

| # | Surface | Routes | Covered by CLI? | Covered by console? |
|---|---|---|---|---|
| 1 | **Model connections + model routes** (+ revisions, publish) | 10 | no | no |
| 2 | **Agents** (+ revisions, publish, run-templates) | 8 | no | read-only diff component exists (`AgentDiff.tsx`) |
| 3 | **Tools + tool sets** (+ revisions, publish) | 8 | no | no |
| 4 | **Approvals** | via responses/sessions | no | **yes** (`ApprovalPanel.tsx`) |
| 5 | **Runs / responses / sessions** (read, stream) | 10 | `palai response` | **yes** (`/runs`, `Timeline.tsx`) |
| 6 | **API keys, orgs, projects** | 11 | **yes** (`palai org/project/apikey`) | no |
| 7 | **Secret refs** (+ rotate) | 4 | **yes** (`palai secret`) | no |
| 8 | MCP connections (+ discover) | 4 | no | no |
| 9 | Slack connections (CRUD + revise) | 5 | no | no |
| 10 | Knowledge bases + sources + ingest + query + index revisions | 8 | no | no |
| 11 | Skills (+ revisions, enable) | 4 | no | no |
| 12 | Triggers (+ revisions, deliveries) and schedules | 16 | no | no |
| 13 | Webhook endpoints + deliveries + redeliver | 5 | no | no |
| 14 | Budgets, quotas, usage, ledger | 6 | no | no |
| 15 | Repository bindings | 3 | no | no |
| 16 | Queue connections | 2 | no | no |
| 17 | Hooks | 2 | no | no |
| 18 | A2A remote agents | mount | no | no |
| 19 | Artifacts (read + content) | 3 | no | download hardening tested |

### 5.2 What a first-run user MUST have UI for

The test is not "is it important" — it is **"can a new operator reach their first successful run without it, and can they trust what they did?"**

**MUST have UI (four surfaces):**

1. **Model connections + model routes.** Non-negotiable and currently the worst gap on the list. Nothing runs until a route is created *and a revision is published*. A revise-then-publish workflow expressed as raw `curl` is where first-run users give up. This is the single highest-value screen in the product.
2. **Agents + revisions + publish.** The product is agents. The same revise/publish two-step applies. A read-only `AgentDiff` component already exists and is the right half of this screen.
3. **Tool sets.** An agent with no tools does nothing interesting. Selection-and-publish, which is exactly the kind of thing nobody wants to hand-write JSON for.
4. **Secrets and API keys.** The CLI covers these, which is *nearly* enough — but a provider API key is the very first secret anyone creates, on the same screen where they create the model connection. Splitting that across UI and CLI is a bad first ten minutes.

**Should have, but survivable via CLI/API for v1:** budgets and quotas (money — people want to see it, but a `GET /v1/usage` is legible), MCP connections, Slack connections.

**Can stay API-only:** knowledge bases, skills, hooks, triggers, schedules, webhooks, repository bindings, queue connections, A2A. These are all *second-week* surfaces — an operator reaches them after the product has already proven itself, and by then `curl` is acceptable.

### 5.3 What comparable products ship as their first admin UI

n8n, Dify, Langfuse and Supabase are all **UI-first**: the web application *is* the product and the API is secondary. That is the wrong comparison for Palai, which is API-first by design. **Temporal is the right comparison** — its Web UI is primarily observability (workflows, event histories, search) with a thin slice of operational actions, and the configuration surface stays in `tctl`/`temporal` CLI and config files. Nobody considers Temporal unusable for it.

**So the honest target is Temporal-shaped, not n8n-shaped**: observability plus the handful of configuration surfaces a first-run user cannot avoid. Palai already has the observability half (`/runs`, timeline, approvals). What is missing is the four MUST surfaces above — **and a CLI that covers everything else**, which is the cheaper half and is currently four verbs deep.

### 5.4 Rough size

**Flagged as an estimate; I did not attempt it.** The console is Next.js with an established public-API-only relay (`app/api/palai/v1/[...path]/route.ts`) and reusable `Panel`/`Status` components, so the plumbing is done. Four resource areas, each needing list + detail + create + revise-and-publish, plus a settings shell and navigation:

- **~6–8 new pages**, reusing the relay and the existing components.
- The revise-and-publish pattern is identical across agents, tools, tool sets and model routes — it is **one component built four times**, which is the cheap kind of work.
- No new backend: every route already exists, and the console's public-API-only constraint (browser can only address `/v1/*`, proven both ends) is already enforced and must not be broken.
- The console is `preview` tier and stays there regardless (§6 leg 8: every console proof ran against a fake `/v1` upstream, never a real control plane). **Adding pages does not move the tier.** Deploying the console against a real control plane does.

Extending the admin CLI to cover the remaining surfaces is probably the **better first move** — same coverage, a fraction of the effort, and it makes the API-only surfaces documentable in the runbooks.

---

## 6. Question 5 — sequencing: what actually stands between today and a stranger running Palai

Ranked by **whether it blocks adoption**, not by how interesting it is. Note that items 1, 2, 4 and 5 are *already booked* in `known-gaps-1.0.md` as operator legs `L-2`, `L-3`, `L-1` and `L-8b` — this list is a re-ranking of that table by buyer impact, not a new discovery.

### Blocks adoption absolutely — a stranger cannot proceed

**1. Publish the artifacts.** (`L-2`)
Images to a registry (GHCR or Docker Hub) with real tags; the CLI as a GitHub Release binary per platform; `@palai/sdk` to npm with `"private"` removed. Without this there is no product, only a repository. Everything else on this list is optional by comparison. The release pipeline already builds, signs, SBOMs, provenance-attests and verifies all twelve artifacts — **the last mile is credentials and a non-dry-run step**, not new machinery.

**2. Make install work without the source tree.**
Today `palai` resolves `deploy/compose/compose.yaml` relative to cwd, and `install.md` starts with `go build`. Fix: embed the compose files in the CLI binary (or ship them in the release tarball), and default `PALAI_*_IMAGE` to the published tags rather than `build:` contexts. Small, and worthless without item 1 — do them as one piece of work.

### Blocks adoption for most evaluators

**3. A real amd64 UAT run.** (`L-3`)
91% of the market. Buy an amd64 runner, run the existing `make uat-*` targets, record the transcripts, let the matrix guard flip the cells honestly. Nothing needs porting — I checked.

**4. First-run configuration without `curl`.**
Minimum: model connections and model routes. Cheapest path is **CLI first** (`palai model connection create`, `palai model route create/publish`) — days rather than weeks, and it makes the runbooks copy-pasteable. UI after.

**5. A cloud VM install with a real domain and a real certificate.** (`L-8a`)
`install.md` states its own ceiling: every step was verified against a local production-compose bring-up on Docker Desktop with a self-minted CA. The step-7 ACME swap has never been executed. This is a one-afternoon operator leg that converts the install doc from "verified locally" to "verified where buyers deploy".

### Blocks the flagship use case specifically

**6. Ship the macOS capability worker.**
Add `palai-capability-worker` to `scripts/release/build.sh` for `darwin/arm64`, wire `internal/workers` into `palai-control-plane/main.go` so the gateway has a production importer, add a Mac-worker row to the support matrix, document the enrolment. Without this the iOS story has infrastructure and no delivery.

**7. Get an Apple Developer account and turn `apple-build` on.** (`L-8d`)
Signing material is the blocker; it is a purchase and a ceremony, not engineering. Until then the flagship demo ends at "compiles and runs in the Simulator" and cannot ship a signed build.

**8. Decide the Mac hosting posture and price it into the plan.**
Recommendation: **one dedicated Scaleway or MacStadium Mac mini M4 per customer, ~€149–$149/month.** Do not build a bursting allocator — the 24-hour licence floor makes the break-even ~28–30 session-days a month and any real customer crosses that immediately.

### Blocks trust for larger buyers, not first contact

**9. A real CI release run** — protected environment, two maintainers, workflow identity, Sigstore/KMS signature, transparency log. (`L-1`) The `ApprovalGate` recomputes from CODEOWNERS and currently *always refuses* with one owner, by design; this needs a second human.
**10. A real Kubernetes install with an enforcing CNI.** (`L-8b`) Today's default-deny is proven as a render on kind, not as enforcement.
**11. The remaining admin UI**, KMS-backed master key (`L-6`), reference-hardware performance numbers, real-model eval quality (`L-5`).

---

## 7. Decision table

| # | Do this | What it buys | What it costs | If you skip it |
|---|---|---|---|---|
| 1 | **Publish images + CLI binaries + npm SDK** | Palai becomes obtainable. This is the difference between a product and a repository. | Registry credentials, a signing key that lives somewhere real, flipping `publish-dryrun.sh` to the real thing. The build/sign/verify chain already exists. | **Nobody can ever install it.** Nothing else on this table matters. |
| 2 | **Install without a source checkout** (embed compose, default to published tags) | `palai init && palai up` works from a downloaded binary. | Small. Embed the two compose files, change the image defaults. | Item 1 ships images nobody can point the CLI at. |
| 3 | **Full UAT run on amd64** (`L-3`) | Four support-matrix cells flip to `tested` on the architecture 91% of clusters use. | An amd64 Docker host (≈€16/month) + making the UAT scripts run non-interactively there. **No portability work — verified.** | Every serious evaluator reads the matrix, sees their architecture untested, and leaves without telling you. |
| 4 | **CLI (then UI) for model connections + routes** | A new operator reaches their first run without `curl`. | CLI: days. UI: ~6–8 pages on the existing relay. | First-run is a JSON-over-`curl` exercise. Most evaluations end here. |
| 5 | **Cloud VM install, real domain, ACME cert** (`L-8a`) | `install.md` stops being "verified on Docker Desktop". | One afternoon on a €16/month VM. | The install guide's step 7 stays unexecuted and the first buyer discovers it for you. |
| 6 | **Ship `palai-capability-worker` for darwin + wire the gateway into main** | The iOS use case has a delivery mechanism. | Add a build target; import `internal/workers` in the control plane's main; a support-matrix row. | The flagship demo cannot run on a customer's Mac at all. |
| 7 | **Apple Developer account, enable `apple-build`** (`L-8d`) | Signed builds; the PR-opening story completes. | A purchase and a signing ceremony. Not engineering. | The demo stops at the Simulator. |
| 8 | **Commit to one dedicated Mac per customer** (~€149/mo) | Isolation for free, predictable COGS, no allocator to build. | ~€170/month per customer all-in with the control plane. | Either an unsafe shared Mac (see the isolation research) or a bursting allocator that saves nothing above ~28 session-days/month. |
| 9 | **Real CI release run, two maintainers** (`L-1`) | Provenance a security reviewer will accept. | A second human with commit rights. The `ApprovalGate` refuses with one owner *by design*. | Enterprise procurement stalls. Does not block first contact. |
| 10 | **Real cluster with an enforcing CNI** (`L-8b`) | Default-deny proven as enforcement, not as a render. | A managed cluster for a day. | The NetworkPolicy claim stays honest-but-weak. Only sophisticated buyers notice. |
| 11 | **Remaining admin UI, KMS master key, eval numbers** | Polish, and the surfaces that make Palai pleasant rather than possible. | Weeks. | Nothing about adoption. Do these after someone is actually using it. |

---

## 8. What I could not verify

Stated so nothing here is mistaken for measurement:

- **DigitalOcean, Azure and GCP default instance architecture** for a first-time user — not established.
- **AKS arm64 node-pool support** in 2026 — no authoritative current source found.
- **Hetzner CAX (arm64) availability in US regions** — two sources contradict each other (Ashburn yes vs EU-only). Hillsboro is x86-only in both.
- **Mac mini M4 retail hardware price** — not fetched; the self-rack capex line is therefore incomplete.
- **MacStadium Orka virtualisation pricing** — sales-contact only.
- **EUR/USD rate** — deliberately not applied; all prices are quoted in the vendor's own currency.
- **The effort estimate for the amd64 UAT run and for the admin UI** — both are my judgement, not measurement. I did not attempt either.
