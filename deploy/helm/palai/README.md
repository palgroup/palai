# Palai control-plane Helm chart (E15 T3)

A restricted Helm chart that installs the Palai self-host **control-plane** on Kubernetes.

## What this chart deploys — and what it deliberately does NOT

**The chart templates what EXISTS today, not the spec's eventual decomposition (plan §2).** Palai
is currently ONE control-plane binary; this chart renders exactly that plus the policies around it:

| Rendered | Purpose |
|---|---|
| `Deployment` (control-plane, `replicas: 1`) | the single binary — API `:8080` + runner gateway `:8443` |
| `Job` (pre-install/pre-upgrade **hook**) | runs `palai-control-plane --migrate-and-exit` BEFORE the Deployment rolls |
| `Service` ×2 | the API and the runner gateway |
| `Ingress` (optional, TLS, path `/v1` only) | fronts the API |
| `NetworkPolicy` ×2 | default-deny + a scoped allow |
| `PodDisruptionBudget` | guards the replica against voluntary disruption |
| `ServiceAccount` + namespace `Role`/`RoleBinding` | **ZERO ClusterRole** — no cluster-admin |

It does **NOT**:

- **Invent coordinator/worker/console components.** There is one binary; the chart renders one Deployment.
- **Run an in-cluster database.** External PostgreSQL and S3 are **config only**, referenced through an
  existing `Secret` (`postgres.existingSecret`, `s3.existingSecret`). The chart never renders a
  `StatefulSet` or a DB pod.
- **Include the runner.** See below.
- **Claim HA.** `replicas: 1`, no HA statement (plan §45.2). Real HA (replicas>1 + topology spread +
  a real load balancer) is the operator leg (§6).

## The runner is NOT in this chart (plan §45.4)

The Palai runner holds the **host Docker socket** to supervise the hardened engine sandbox. Putting a
docker.sock-holding pod inside the cluster would defeat the isolation boundary, so the runner is
installed from the **signed E14 host package** and runs **OUTSIDE** the cluster. It enrolls
**outbound-only**: it dials the control-plane's runner gateway (`:8443`) — the gateway opens no
connection back to the runner.

To connect a runner to a chart-installed control-plane:

1. Expose the runner gateway Service so the runner can reach it (a `LoadBalancer`, or `NodePort` +
   `runnerGateway.service.nodePort`). The gateway's server certificate pins exactly ONE SAN —
   `control-plane` — so the runner MUST dial with `PALAI_CONTROLLER_DNS=control-plane` regardless of the
   address it connects to (the address is only used for the TCP dial; TLS verifies the SAN).
2. Provision the runner's CA cert, server cert/key, and a one-use enrollment token into the
   `runnerGateway.existingSecret` (see below).
3. On the runner host, install the E14 signed package and set `PALAI_CONTROLLER_URL=https://<addr>:<port>`,
   `PALAI_CONTROLLER_DNS=control-plane`, `PALAI_RUNNER_CA_CERT=<the CA cert>`, and the one-use token.

See `docs/operations/kubernetes.md` for the full walkthrough.

## Required inputs

Every credential rides an **existing Secret** — nothing sensitive appears in `values.yaml`,
ConfigMaps, or `helm get values`.

| Value | Secret contents |
|---|---|
| `postgres.existingSecret` | key `database-url` → `postgres://user:pass@host:5432/db?sslmode=require` |
| `s3.existingSecret` + `s3.endpoint` | keys `access-key`, `secret-key` |
| `runnerGateway.existingSecret` | keys `ca.crt`, `ca.key`, `server.crt`, `server.key`, `runner-token` |
| `bootstrap.existingSecret` (optional) | key `bootstrap-api-key` (seeded on first boot) |
| `secretStore.existingSecret` (optional) | key `master-key` (32-byte hex; enables the DB-backed secret store) |

`postgres.existingSecret` is **required** — `helm template` fails fast without it, so a broken install
is caught at render time.

## Install

```sh
helm install palai deploy/helm/palai \
  --namespace palai --create-namespace \
  --set engineImage=palai/reference-engine@sha256:... \
  --set postgres.existingSecret=palai-db \
  --set s3.endpoint=https://s3.example.com --set s3.existingSecret=palai-s3 \
  --set runnerGateway.existingSecret=palai-runner-gw \
  --set bootstrap.existingSecret=palai-bootstrap
```

The pre-install migration Job applies the schema, then the control-plane Deployment starts and passes
its `/healthz` readiness probe. Provision tenants with the `palai` admin CLI over the API (Ingress or a
port-forward), then enroll a runner from the host.

## Security posture

- **Pod Security "restricted"**: `runAsNonRoot`, non-zero uid, `allowPrivilegeEscalation: false`,
  `capabilities.drop: [ALL]`, `seccompProfile: RuntimeDefault`, `readOnlyRootFilesystem: true`
  (a `/tmp` emptyDir is mounted for scratch). Deploy into a namespace labelled
  `pod-security.kubernetes.io/enforce=restricted`.
- **Namespace-scoped RBAC only** — **ZERO ClusterRole**. The control-plane makes no Kubernetes API
  calls; the ServiceAccount disables token automounting and the Role ships with no rules.
- **NetworkPolicy** default-deny + a scoped allow (DNS, external PG/S3 egress, ingress-controller →
  API, runner-gateway ingress).

### HONEST CEILING (plan §T3)

- **kind's default CNI (kindnet) does NOT enforce NetworkPolicy.** This chart's policies are proven
  CORRECT at the render/schema level (`tests/uat/kubernetes`) and installed on a kind cluster
  (`kind-smoke.sh`), but **enforcement** — default-deny actually dropping traffic — requires a
  policy-enforcing CNI (Calico/Cilium) on a real cluster. That, real EKS/GKE, HA/topology-spread, and
  real LB/cert-manager are the **operator leg** (§6).
- The kind smoke proves lint + render + policy-assert + a **local install**. It does not prove real
  managed Kubernetes.
