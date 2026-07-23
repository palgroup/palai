#!/usr/bin/env bash
# E15 T3 — kind install smoke for the restricted Helm chart (deploy/helm/palai).
#
# It proves the chart INSTALLS and RUNS on a local kind cluster (kind nodes are Docker containers;
# they run on Docker Desktop):
#
#   1. Create a UNIQUE kind cluster (extraPortMappings expose the API + runner-gateway NodePorts).
#   2. Build the control-plane + reference-engine images on the host; `kind load` the control-plane
#      (the runner runs on the HOST, so the engine image stays on the host).
#   3. Deploy chart-EXTERNAL PostgreSQL + S3 fixtures into the cluster; the chart's external PG/S3
#      values point at them (proving external-store config, NOT an in-cluster DB in the chart).
#   4. `palai init` mints the runner-gateway CA + server cert (SAN "control-plane"); load the TLS
#      material + a one-use runner token + a bootstrap key into Secrets.
#   5. `helm install` — the pre-install migration Job (--migrate-and-exit) completes, then the
#      control-plane Deployment rolls and /healthz goes green.
#   6. Provision org -> project -> api-key through the `palai` admin CLI over the API NodePort.
#   7. Enroll the runner from the SIGNED E14 host package as a host-side container dialing the
#      gateway NodePort outbound-only (SAN pinned "control-plane").
#   8. Create a fake-provider response and poll it to `completed` — only the enrolled runner could
#      have run it.
#
# HONEST CEILING (plan §T3): kind's default CNI (kindnet) does NOT enforce NetworkPolicy — the
# chart's default-deny/allow policies are INSTALLED here and proven CORRECT at render/schema level
# (tests/uat/kubernetes render-asserts), but ENFORCEMENT needs a policy-enforcing CNI on a real
# cluster. Real EKS/GKE, HA/topology-spread, real LB/cert-manager = operator leg (§6). This smoke
# proves lint+render+policy-assert + a kind-LOCAL install; it does NOT prove real managed Kubernetes.
set -euo pipefail

for bin in docker kind helm kubectl go openssl; do
	command -v "$bin" >/dev/null 2>&1 || { echo "kind-smoke: '$bin' is required" >&2; exit 2; }
done

root="$(git rev-parse --show-toplevel)"
short="$(openssl rand -hex 3)"
cluster="palai-e15t3-${short}"          # unique so concurrent sibling tasks don't collide
runner_ctr="palai-e15t3-${short}-runner"
ns="palai"
work="$(mktemp -d)"
export PALAI_HOME="$work/home"
palai="$work/palai"
# Random high host ports for the two NodePorts, so concurrent kind clusters don't fight over them.
api_hp=$(( 20000 + RANDOM % 20000 ))
gw_hp=$(( 20000 + RANDOM % 20000 ))
while [ "$gw_hp" = "$api_hp" ]; do gw_hp=$(( 20000 + RANDOM % 20000 )); done
api_np=30080
gw_np=30443

# Chart-external fixture images: the compose-proven digests, tagged locally + loaded into kind so
# nothing pulls inside the cluster.
pg_ref="postgres@sha256:17e67d7b9890c99b055ba1e0d5c5be4ec27c9d3a72bda32db24a5e5d8a85af0c"
s3_ref="docker.io/chrislusf/seaweedfs@sha256:c7d6c721b30ae711db766bbbfd40192776e263d4e51e22f57baef7bef93c12c6"
pg_local="palai-e15t3-fixture/postgres:${short}"
s3_local="palai-e15t3-fixture/seaweedfs:${short}"

cleanup() {
	set +e
	echo "--- cleanup ---" >&2
	docker rm -f "$runner_ctr" >/dev/null 2>&1
	kind delete cluster --name "$cluster" >/dev/null 2>&1
	docker ps -aq --filter "label=io.palai.project=$cluster" | xargs -r docker rm -f >/dev/null 2>&1
	rm -rf "$work"
}
trap cleanup EXIT

step() { echo "==> $*" >&2; }

# dump_ns prints namespace diagnostics BEFORE the cleanup trap deletes the cluster, so a real failure
# surfaces its reason (a timed-out rollout otherwise vanishes with the cluster).
dump_ns() {
	echo "--- diagnostics ($1) ---" >&2
	kubectl -n "$ns" get pods -o wide >&2 2>&1 || true
	kubectl -n "$ns" get events --sort-by=.lastTimestamp 2>&1 | tail -25 >&2 || true
	for d in palai-pg-fixture palai-s3-fixture palai; do
		kubectl -n "$ns" logs "deploy/$d" --tail=20 >&2 2>&1 || true
	done
}

step "build palai CLI"
( cd "$root" && go build -o "$palai" ./cmd/cli )

step "palai init (mints runner-gateway CA + server cert, SAN control-plane)"
"$palai" init >&2
runner_token="$(openssl rand -hex 24)"
bootstrap_key="palai-e15t3-$(openssl rand -hex 16)"

step "build control-plane + reference-engine images"
docker build -q -t palai/control-plane:local -f "$root/deploy/compose/control-plane.Dockerfile" "$root" >&2
docker build -q -t palai/reference-engine:local "$root/engines/reference" >&2
engine_digest="$(docker image inspect palai/reference-engine:local --format '{{.Id}}')"

step "pull + tag + load fixture images"
docker image inspect "$pg_ref" >/dev/null 2>&1 || docker pull -q "$pg_ref" >&2
docker image inspect "$s3_ref" >/dev/null 2>&1 || docker pull -q "$s3_ref" >&2
docker tag "$pg_ref" "$pg_local"
docker tag "$s3_ref" "$s3_local"

step "create kind cluster $cluster (API :$api_hp, runner gateway :$gw_hp)"
cat >"$work/kind.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: $cluster
nodes:
  - role: control-plane
    extraPortMappings:
      - containerPort: $api_np
        hostPort: $api_hp
        protocol: TCP
      - containerPort: $gw_np
        hostPort: $gw_hp
        protocol: TCP
EOF
kind create cluster --config "$work/kind.yaml" --wait 120s >&2

step "load images into the cluster"
kind load docker-image palai/control-plane:local "$pg_local" "$s3_local" --name "$cluster" >&2

step "create namespace (restricted Pod Security enforced) + fixtures + secrets"
kubectl create namespace "$ns" >&2
kubectl label namespace "$ns" pod-security.kubernetes.io/enforce=restricted --overwrite >&2

# Chart-EXTERNAL PostgreSQL + S3 fixtures. postgres runs as non-root (uid 999 is the postgres user)
# to satisfy restricted PSS; seaweedfs runs as a non-root uid. These are NOT part of the chart — they
# stand in for the operator's managed PG/S3.
cat >"$work/fixtures.yaml" <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: { name: palai-pg-fixture, namespace: $ns }
spec:
  replicas: 1
  selector: { matchLabels: { app: palai-pg-fixture } }
  template:
    metadata: { labels: { app: palai-pg-fixture } }
    spec:
      securityContext: { runAsNonRoot: true, runAsUser: 999, fsGroup: 999, seccompProfile: { type: RuntimeDefault } }
      containers:
        - name: postgres
          image: $pg_local
          imagePullPolicy: Never
          securityContext:
            allowPrivilegeEscalation: false
            runAsNonRoot: true
            capabilities: { drop: [ALL] }
          env:
            - { name: POSTGRES_USER, value: palai }
            - { name: POSTGRES_PASSWORD, value: palai }
            - { name: POSTGRES_DB, value: palai }
            - { name: PGDATA, value: /var/lib/postgresql/data/pgdata }
          ports: [ { containerPort: 5432 } ]
          volumeMounts: [ { name: data, mountPath: /var/lib/postgresql/data } ]
          readinessProbe:
            exec: { command: ["pg_isready","-U","palai","-d","palai"] }
            periodSeconds: 2
      volumes: [ { name: data, emptyDir: {} } ]
---
apiVersion: v1
kind: Service
metadata: { name: palai-pg-fixture, namespace: $ns }
spec:
  selector: { app: palai-pg-fixture }
  ports: [ { port: 5432, targetPort: 5432 } ]
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: palai-s3-fixture, namespace: $ns }
spec:
  replicas: 1
  selector: { matchLabels: { app: palai-s3-fixture } }
  template:
    metadata: { labels: { app: palai-s3-fixture } }
    spec:
      securityContext: { runAsNonRoot: true, runAsUser: 1000, fsGroup: 1000, seccompProfile: { type: RuntimeDefault } }
      containers:
        - name: seaweedfs
          image: $s3_local
          imagePullPolicy: Never
          args: ["server","-s3","-dir=/data"]
          securityContext:
            allowPrivilegeEscalation: false
            runAsNonRoot: true
            capabilities: { drop: [ALL] }
          ports: [ { containerPort: 8333 } ]
          volumeMounts: [ { name: data, mountPath: /data } ]
          readinessProbe:
            httpGet: { path: /, port: 8333 }
            periodSeconds: 2
      volumes: [ { name: data, emptyDir: {} } ]
---
apiVersion: v1
kind: Service
metadata: { name: palai-s3-fixture, namespace: $ns }
spec:
  selector: { app: palai-s3-fixture }
  ports: [ { port: 8333, targetPort: 8333 } ]
EOF
kubectl apply -f "$work/fixtures.yaml" >&2

step "wait for the external PG + S3 fixtures"
kubectl -n "$ns" rollout status deploy/palai-pg-fixture --timeout=180s >&2 || { dump_ns "pg fixture"; exit 1; }
kubectl -n "$ns" rollout status deploy/palai-s3-fixture --timeout=180s >&2 || { dump_ns "s3 fixture"; exit 1; }

ca_dir="$PALAI_HOME/ca"
kubectl -n "$ns" create secret generic palai-db \
	--from-literal=database-url="postgres://palai:palai@palai-pg-fixture:5432/palai?sslmode=disable" >&2
kubectl -n "$ns" create secret generic palai-s3 \
	--from-literal=access-key="palai-e15t3" --from-literal=secret-key="palai-e15t3-secret" >&2
kubectl -n "$ns" create secret generic palai-runner-gw \
	--from-file=ca.crt="$ca_dir/ca.crt" --from-file=ca.key="$ca_dir/ca.key" \
	--from-file=server.crt="$ca_dir/server.crt" --from-file=server.key="$ca_dir/server.key" \
	--from-literal=runner-token="$runner_token" >&2
kubectl -n "$ns" create secret generic palai-bootstrap \
	--from-literal=bootstrap-api-key="$bootstrap_key" >&2

step "helm install (pre-install migration Job -> control-plane)"
helm install palai "$root/deploy/helm/palai" --namespace "$ns" --wait --timeout 5m \
	--set image.pullPolicy=Never \
	--set engineImage="$engine_digest" \
	--set postgres.existingSecret=palai-db \
	--set s3.endpoint="http://palai-s3-fixture:8333" \
	--set s3.port=8333 \
	--set s3.existingSecret=palai-s3 \
	--set runnerGateway.existingSecret=palai-runner-gw \
	--set runnerGateway.certTTL=5m \
	--set runnerGateway.service.type=NodePort \
	--set runnerGateway.service.nodePort=$gw_np \
	--set service.type=NodePort \
	--set service.nodePort=$api_np \
	--set bootstrap.existingSecret=palai-bootstrap >&2 || { dump_ns "helm install"; exit 1; }

step "prove the migration Job ran as a pre-install hook and completed"
if ! kubectl -n "$ns" get job palai-migrate -o jsonpath='{.status.succeeded}' 2>/dev/null | grep -q 1; then
	# Hook Jobs with hook-succeeded delete-policy are reaped after success; the release history is the
	# durable proof. Confirm the release is deployed (the hook must have succeeded for install to proceed).
	helm -n "$ns" status palai | grep -q "STATUS: deployed" || { echo "migration hook did not complete" >&2; exit 1; }
fi
echo "migration hook completed (release deployed)" >&2

step "wait for /healthz over the API NodePort"
base="http://127.0.0.1:$api_hp"
healthy=false
for _ in $(seq 1 60); do
	if curl -fsS "$base/healthz" >/dev/null 2>&1; then healthy=true; break; fi
	sleep 2
done
$healthy || { echo "control-plane /healthz never went green" >&2; kubectl -n "$ns" get pods >&2; kubectl -n "$ns" logs deploy/palai --tail=50 >&2; exit 1; }
echo "healthz green" >&2

step "provision org -> project -> api-key through the admin CLI over the API NodePort"
bootstrap_file="$work/bootstrap-key"
printf '%s' "$bootstrap_key" >"$bootstrap_file"
admin=( "$palai" --base-url "$base" --api-key-file "$bootstrap_file" --json )
org_id="$("${admin[@]}" org create --display-name "E15T3 Kind" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
prj_id="$("${admin[@]}" project create --display-name "kind" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
key_id="$("${admin[@]}" apikey create --project "$prj_id" --scope run | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
[ -n "$org_id" ] && [ -n "$prj_id" ] && [ -n "$key_id" ] || { echo "admin CLI provisioning did not mint ids (org=$org_id prj=$prj_id key=$key_id)" >&2; exit 1; }
echo "provisioned org=$org_id project=$prj_id apikey=$key_id" >&2

step "enroll the SIGNED E14 runner package from the host (outbound-only, SAN control-plane)"
pkgout="$work/pkg"; extract="$work/extract"
OUT="$pkgout" ARCH="$(docker version --format '{{.Server.Arch}}')" bash "$root/scripts/package/runner/build.sh" >/dev/null
tarball="$(cd "$pkgout" && ls palai-runner-host-*.tar.gz)"
( cd "$pkgout" && ./verify.sh "$tarball" palai-runner-signing.pub ) >&2
mkdir -p "$extract"; tar -xzf "$pkgout/$tarball" -C "$extract"
printf '%s' "$runner_token" >"$work/runner-token"
docker run -d --name "$runner_ctr" --label "io.palai.project=$cluster" \
	--add-host host.docker.internal:host-gateway \
	-v /var/run/docker.sock:/var/run/docker.sock \
	-v "$extract:/opt/palai-runner:ro" \
	-v "$ca_dir/ca.crt:/palai/ca.crt:ro" \
	-v "$work/runner-token:/palai/runner-token:ro" \
	-e PALAI_CONTROLLER_URL="https://host.docker.internal:$gw_hp" \
	-e PALAI_CONTROLLER_DNS=control-plane \
	-e PALAI_RUNNER_ID=runner-e15t3 \
	-e PALAI_RUNNER_DNS=runner-e15t3.runners.palai.internal \
	-e PALAI_RUNNER_CA_CERT=/palai/ca.crt \
	-e PALAI_ENROLLMENT_TOKEN_FILE=/palai/runner-token \
	-e PALAI_ENGINE_IMAGE="$engine_digest" \
	--entrypoint /opt/palai-runner/palai-runner.sh \
	alpine:3.21 >/dev/null

enrolled=false
for _ in $(seq 1 60); do
	if docker logs "$runner_ctr" 2>&1 | grep -q "enrolled runner"; then enrolled=true; break; fi
	if [ "$(docker inspect -f '{{.State.Running}}' "$runner_ctr" 2>/dev/null)" != "true" ]; then
		echo "runner exited early:" >&2; docker logs "$runner_ctr" >&2; exit 1
	fi
	sleep 2
done
$enrolled || { echo "runner did not enroll:" >&2; docker logs "$runner_ctr" >&2; exit 1; }
docker logs "$runner_ctr" 2>&1 | grep "enrolled runner" >&2

step "create a fake-provider response and await terminal"
created="$(curl -fsS -X POST "$base/v1/responses" \
	-H "Authorization: Bearer $bootstrap_key" -H "Content-Type: application/json" \
	-H "Idempotency-Key: $(openssl rand -hex 16)" \
	-d '{"input":"hello from kind"}')"
id="$(printf '%s' "$created" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
echo "response id=$id" >&2
status=""
for _ in $(seq 1 120); do
	body="$(curl -fsS "$base/v1/responses/$id" -H "Authorization: Bearer $bootstrap_key")"
	status="$(printf '%s' "$body" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",""))')"
	case "$status" in completed|failed|canceled) break ;; esac
	sleep 1
done
echo "==> terminal status: $status" >&2
if [ "$status" != "completed" ]; then
	echo "KIND SMOKE FAILED: response did not complete (status=$status)" >&2
	docker logs "$runner_ctr" 2>&1 | tail -30 >&2
	kubectl -n "$ns" logs deploy/palai --tail=50 >&2
	exit 1
fi

echo "KIND SMOKE PASSED: chart installed on kind $cluster (migration hook + restricted CP + external PG/S3), runner enrolled outbound-only from the host, response $id completed."
