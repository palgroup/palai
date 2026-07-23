# Kubernetes install (restricted Helm chart)

Palai's control-plane installs on Kubernetes via the `deploy/helm/palai` chart. This page is the
operator runbook; the chart's own `README.md` is the reference for every value.

Everything here is built and proven in-repo against a **kind** cluster (kind nodes are Docker
containers; they run on Docker Desktop). The **operator ceiling** (plan §6) is a real managed cluster
(EKS/GKE) with a policy-enforcing CNI, real ingress/LB/cert-manager, and PSS-restricted admission —
none of which runs in the macOS + Docker Desktop session that builds this.

## What the chart is (and is not)

The chart templates **what exists today**: ONE control-plane binary as a Deployment, a migration Job
hook, and the namespace-scoped policies around them (plan §2). It runs **no in-cluster database**
(external PostgreSQL/S3 are config-only) and it does **not** include the runner.

## The runner runs OUTSIDE the cluster

The runner holds the **host Docker socket** to supervise the hardened engine sandbox. A
docker.sock-holding pod inside the cluster would break that isolation boundary, so — exactly as in the
split-VM leg (E14 T5) — the runner is installed from the **signed host package** on a host outside the
cluster and enrolls **outbound-only** into the control-plane's runner gateway.

```
   ┌─────────────── Kubernetes cluster ───────────────┐
   │  control-plane Deployment                          │
   │   ├─ API           :8080  ──(Ingress /v1, TLS)──┐  │
   │   └─ runner gateway:8443  ◄──────────────────┐  │  │
   └───────────────────────────────────────────────│──│──┘
                                                    │  │
      runner host (OUTSIDE the cluster) ────────────┘  │  outbound-only mTLS,
        holds docker.sock, dials PALAI_CONTROLLER_URL   │  SAN pinned "control-plane"
```

The runner gateway's server certificate carries exactly ONE SAN — `control-plane` — because the
runner session pins the controller's DNS identity exactly. The runner therefore always dials with
`PALAI_CONTROLLER_DNS=control-plane`; the address in `PALAI_CONTROLLER_URL` is used only for the TCP
connection, while TLS verifies the pinned SAN.

## Prerequisites

- An external PostgreSQL and an S3-compatible object store the cluster pods can reach.
- Secrets provisioned in the target namespace (see the chart README's "Required inputs" table).
- The runner-gateway TLS material — a CA cert+key, a server cert+key with SAN `control-plane`, and a
  one-use enrollment token. `palai init` mints a compatible CA + server cert locally
  (`cmd/cli/internal/stack/certs.go`); load them into `runnerGateway.existingSecret`.
- The namespace labelled for restricted Pod Security:
  ```sh
  kubectl create namespace palai
  kubectl label namespace palai pod-security.kubernetes.io/enforce=restricted
  ```

## Install

```sh
helm install palai deploy/helm/palai --namespace palai \
  --set engineImage=palai/reference-engine@sha256:... \
  --set postgres.existingSecret=palai-db \
  --set s3.endpoint=https://s3.example.com --set s3.existingSecret=palai-s3 \
  --set runnerGateway.existingSecret=palai-runner-gw \
  --set bootstrap.existingSecret=palai-bootstrap
```

The pre-install Job runs `palai-control-plane --migrate-and-exit` and must complete before the
Deployment rolls. Watch it:

```sh
kubectl -n palai get jobs
kubectl -n palai logs job/palai-migrate
kubectl -n palai rollout status deploy/palai
```

## Enroll a runner from the host

1. Expose the runner gateway (`runnerGateway.service.type=LoadBalancer`, or `NodePort` +
   `runnerGateway.service.nodePort`) and note the reachable `<addr>:<port>`.
2. Build + verify the signed runner package (E14): `scripts/package/runner/build.sh`, then `verify.sh`.
3. On the runner host, extract it and set (see `scripts/package/runner/runner.env.example`):
   ```sh
   PALAI_CONTROLLER_URL=https://<addr>:<port>
   PALAI_CONTROLLER_DNS=control-plane
   PALAI_RUNNER_CA_CERT=/path/to/ca.crt
   PALAI_ENROLLMENT_TOKEN_FILE=/path/to/runner-token
   PALAI_ENGINE_IMAGE=palai/reference-engine@sha256:...
   ```
   The runner enrolls outbound-only and holds one mTLS session for leases.

## Provision and run

Provision a tenant with the `palai` admin CLI over the API (through the Ingress, or a
`kubectl port-forward svc/palai 8080:8080`), then create a response — a fake-provider run completes
end to end once a runner is enrolled. This full path is exercised by `tests/uat/kubernetes/kind-smoke.sh`.

## Verification and honest ceiling

- `tests/uat/kubernetes` (Go): `helm lint` + render asserts (ZERO ClusterRole, restricted
  securityContext, NetworkPolicy default-deny, PDB, migration Job hook, external-PG/S3-only) +
  `kubeconform` schema validation. Deterministic, no cluster required.
- `tests/uat/kubernetes/kind-smoke.sh`: a live install on a local kind cluster — `kind load` the images,
  `helm install`, the migration Job completes, `/healthz` green, provision via the admin CLI, enroll a
  host-side runner, a fake-provider run completes. External PG/S3 are chart-EXTERNAL fixtures deployed
  into the cluster.

**Ceiling (plan §T3):** kind's default CNI (**kindnet**) does **NOT enforce NetworkPolicy** — the
policies here are proven CORRECT (render/schema) and installed, but enforcement needs a
policy-enforcing CNI (Calico/Cilium) on a real cluster. Real managed Kubernetes (EKS/GKE),
HA/topology-spread behaviour, and real LB/cert-manager are the **operator leg** (§6).
