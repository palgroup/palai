# Air-gap install (offline, signed bundle) — E15 T4

Palai ships as a single **signed offline bundle** (§45.9) so a facility with no internet can
install and run it. Nothing in the running stack ever reaches out: the images come from a private
registry mirror you populate once, and the stack runs on a Docker network where **egress is
topologically impossible** — not "telemetry disabled by a flag", but *no gateway exists*.

## What's in the bundle

`scripts/release/airgap-build.sh` produces (under `dist/airgap-bundle/`):

| Path | What |
|---|---|
| `images/*.tar` | OCI images by digest (`docker save`): postgres, object-store, control-plane, runner, reference-engine, and `registry:2` (the mirror itself). |
| `runner/` | the E14 signed **runner host package** (its own tarball + detached signature + public key + verifier). |
| `bin/palai-linux-<arch>` | the CLI. |
| `compose/`, `helm/` | the production compose files and the restricted Helm chart (E15 T3). |
| `migrations/` | a copy of `storage/migrations/`. |
| `manifest.json` | version, per-image digests, component list, and the **SBOM / provenance** fields. |
| `sha256sums` | the **signed root**: the sha256 of every file above. |
| `sha256sums.sig`, `sha256sums.sha256`, `palai-airgap-signing.pub` | the openssl ECDSA P-256 **detached signature** over `sha256sums`. |

### Signing — one tool, reused verbatim

The bundle is signed with the **same** `openssl dgst -sha256` / ECDSA P-256 tool the E14 runner
host package uses (`scripts/package/runner/build.sh` + `verify.sh`). There is no second signer.
The bundle even ships a byte copy of that verifier as `runner-verify.sh`, and `verify.sh` execs it
for the top-level signature check. A release passes an operator-held key:

```sh
PALAI_AIRGAP_SIGNING_KEY=/path/to/release-signing.key bash scripts/release/airgap-build.sh
```

With no key set, the build mints an **ephemeral** key so a local proof is self-contained.

### SBOM / provenance — honest naming

`manifest.json` **defines** `sbom` and `provenance` but they are `null` in this RC bundle; each has
a `*_note` saying production lives in **E18**. The fields exist so the shape is stable; they are not
faked.

## 1. Verify — offline, before you trust anything

Obtain **out of band** (the release page / your config management), never from the bundle directory:
the signing **public key** AND the verifying **code** — both `verify.sh` and `runner-verify.sh` (all
three live in the repo, ~80 lines each). A channel attacker can swap the artifacts, the signature,
a sibling key, AND the bundle's verifier (replacing it with `exit 0`) all at once; the out-of-band
key is worthless if the code checking it came from the same channel. Run the out-of-band `verify.sh`
— it PREFERS a `runner-verify.sh` sitting next to it over the bundle's copy.

**Verify on the host first — no Docker, no daemon, and BEFORE any `docker load`** (a `docker load`
hands an untrusted tar to the daemon's parser, so never load until the bundle is verified):

```sh
./verify.sh dist/airgap-bundle /path/to/trusted.pub     # host: openssl + sha256sum only
```

`verify.sh` checks **(1)** the signature over `sha256sums` (E14 T5 verifier verbatim) and **(2)** the
digest chain (`sha256sum -c sha256sums`) — every file matches its signed digest. Any tampered byte
(or a wrong key) fails it **closed**.

To additionally PROVE the check needs no network, re-run it in a container with **no network at all**
(this does `docker load` a tool image, so run it only AFTER the host verify above has passed):

```sh
docker load -i dist/airgap-bundle/images/postgres.tar   # only after the host verify passed
docker tag <loaded-id> airgap-verify:tool
PALAI_AIRGAP_TOOL_IMAGE=airgap-verify:tool \
  ./verify.sh --network-none dist/airgap-bundle /path/to/trusted.pub
```

## 2. Install — mirror + bring up from the mirror

```sh
palai init                       # creates ~/.palai (CA, keys, secrets)
export PALAI_HOME=~/.palai PALAI_AIRGAP_PROJECT=palai PALAI_AIRGAP_REGISTRY_PORT=5000
./dist/airgap-bundle/install.sh dist/airgap-bundle
```

`install.sh` loads the images, pushes them into a private **digest-pinned** `registry:2` mirror on
`127.0.0.1`, **removes the local build tags**, and pulls the stack back **from the mirror by that
digest** — so the running images are provably the mirror's, verified against the signed manifest.
The stack comes up on an `internal: true` network with nothing host-published.

Admit work over the admin CLI / `docker exec` (there is no exposed port by design). A real run
completes with the in-process **fake** provider standing in for a private model endpoint (zero
model egress); the engine sandbox runs `NetworkMode: none` regardless.

## 3. Prove zero egress

```sh
bash deploy/airgap/drill.sh    # build -> offline verify -> tamper FAIL -> mirror install ->
                               # real run completes -> egress attempt FAILS -> in-network git clone
```

The drill asserts the network is `internal: true` (topology) **and** that an egress attempt from a
stack container **fails** — the claim is the topology, not a log line. It also stands up an
in-network git remote (a bare repo served over git's dumb-HTTP protocol) and clones it from another
in-network container, showing a git-dependent workflow works air-gapped with egress still impossible.

## Honest ceiling (plan §6, operator leg)

This is the **local seam**. A real air-gapped facility, the operator **trust-root / mirror
ceremony** (a real registry with real TLS + a real trust root — the 127.0.0.1 mirror here is
loopback-insecure by default), a **real private model server**, and multi-arch images are the
operator leg. SBOM / provenance production is **E18**. For a hardened real deployment, layer the
production overlay (`deploy/compose/production.yml`: TLS edge + master-key guard, E14 T1) on top,
swapping the self-minted edge cert for a real-domain certificate.
