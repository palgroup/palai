# Runner host package (split-VM runner leg)

The runner is Palai's private execution host. In the single-node profile it runs as a compose
service beside the control-plane; in a **split-VM** deployment it runs on its OWN host and dials
the control-plane's runner gateway. This page covers the split-VM leg: the **signed runner host
package** and the **`palai-runner.service`** systemd unit.

Everything here is built and proven in the repo against the local Docker-network seam. The
**operator ceiling** (E14 plan §6) is a real `systemctl enable --now` on a Linux host, boot
persistence, and two physical VMs — none of which runs in the macOS + Docker Desktop session
that builds this. The units are statically verified (`systemd-analyze verify`), the package build
+ signature verify (incl. tamper-FAIL) run here, and the outbound-only enroll+run is proven with
the runner on a SEPARATE Docker network reaching the gateway's published port.

## What the runner does (and does not) expose

- **Outbound-only.** `cmd/runner` opens NO inbound port and activates no socket. It dials
  `PALAI_CONTROLLER_URL` to enroll (https) and holds ONE outbound mTLS session (wss) for leases.
  `palai-runner.service` therefore has no `[Socket]`/`ListenStream` and publishes nothing.
- **The Docker socket reaches the supervisor, never the workload.** The runner needs the host
  Docker socket because it is the trusted supervisor that launches the hardened engine sandbox
  (OCI driver). The untrusted engine receives NO socket and no credential — that isolation is
  proven by `tests/security/runner`. So "no runtime socket to any workload" holds.
- These two invariants are asserted, not just documented:
  `deploy/compose/runner_no_listen_test.go` proves — at the compose-config level — that the
  runner publishes no host port and that the Docker socket is mounted into the runner and NOTHING
  else; `tests/security/runner` proves the engine gets no socket at runtime.

## Build the package

```sh
# From the repo root. Override VERSION/ARCH/OUT as needed; a release sets PALAI_RUNNER_SIGNING_KEY
# to an operator-held ECDSA key (with none set, an ephemeral key is generated for a local proof).
ARCH=amd64 scripts/package/runner/build.sh
```

This writes, under `dist/runner-package/`:

| Artifact | What it is |
|---|---|
| `palai-runner-host-<ver>-linux-<arch>.tar.gz` | the deterministic tarball (binary + unit + launcher + env template + this doc) |
| `…​.tar.gz.sha256` | the sha256 manifest |
| `…​.tar.gz.sig` | the detached `openssl dgst -sha256` signature |
| `palai-runner-signing.pub` | a COPY of the signing public key (convenience only — see the trust model below; do NOT trust this copy) |
| `verify.sh` | the verify script (below) |

The tarball is deterministic: the linux binary is built `-trimpath`, every member is stamped to a
fixed mtime with uid/gid 0, and `gzip -n` drops the header timestamp — two builds of the same
source on the same tar toolchain are byte-identical (`package_test.go`; cross-toolchain identity is
not claimed). The signing tool is `openssl dgst -sha256` over an ECDSA P-256 key: openssl is
already a build dependency (T1 mints the edge/CA certs with it), so no new tool enters the toolchain.

## Verify the package (and the trust model)

The signature is only as good as the public key you check it against. A channel attacker can swap
the tarball, its `.sig`, AND its `.sha256` in one move and re-sign with their own key — so the
`palai-runner-signing.pub` sitting BESIDE the tarball proves nothing. Obtain the real public key
(or its sha256 fingerprint) from a SEPARATE trusted channel — the project's release page, your
config-management, a keyserver — and pass it explicitly. `verify.sh` has no sibling-key default and
fails closed without one.

```sh
# Get the trusted key out of band, then:
./verify.sh palai-runner-host-*.tar.gz /path/to/trusted-signing.pub
# verify: OK — sha256 and signature verified for palai-runner-host-…tar.gz against …

# Optional belt-and-suspenders: pin the key's fingerprint (distribute the 64-hex value out of band).
PALAI_RUNNER_PUBKEY_FINGERPRINT=$(sha256sum trusted-signing.pub | cut -d' ' -f1) \
  ./verify.sh palai-runner-host-*.tar.gz /path/to/trusted-signing.pub
```

`verify.sh` recomputes the tarball sha256 against the manifest AND checks the detached signature
against the supplied key; it fails on ANY mismatch. `package_test.go` proves it: a flipped byte
fails, a flipped byte with a regenerated `.sha256` still fails on the signature, a package re-signed
with a different key fails, and a run with no explicit key fails closed. Always verify before
extracting onto a host.

## Install on the runner host (operator leg)

On a real Linux runner VM (the ceiling — not run in-repo):

```sh
# 0. Create the service user and give it Docker access. NOTE: docker-group membership is
#    root-equivalent on the host — this user is defense-in-depth, not the trust boundary. The real
#    boundary is the OCI engine sandbox the runner supervises (no socket/credential reaches it).
sudo useradd --system --no-create-home --shell /usr/sbin/nologin palai-runner
sudo usermod -aG docker palai-runner

# 1. Verify against the OUT-OF-BAND key (not the .pub in the package), then extract to /opt/palai-runner.
./verify.sh palai-runner-host-*.tar.gz /path/to/trusted-signing.pub
sudo mkdir -p /opt/palai-runner /etc/palai/runner
sudo tar -xzf palai-runner-host-*.tar.gz -C /opt/palai-runner

# 2. Copy the controller CA + a FRESH one-use enrollment token from the control-plane host.
#    (On the control-plane host: cat ${PALAI_HOME}/ca/ca.crt ; mint a token into ${PALAI_HOME}/runner-token.)
sudo install -m 0644 ca.crt        /etc/palai/runner/ca.crt
sudo install -m 0600 runner-token  /etc/palai/runner/runner-token

# 3. Configure and enable.
sudo install -m 0644 /opt/palai-runner/runner.env.example /etc/palai/runner.env   # then edit it
sudo cp /opt/palai-runner/palai-runner.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now palai-runner.service
```

Edit `/etc/palai/runner.env`: set `PALAI_CONTROLLER_URL` (the control-plane VM's gateway, e.g.
`https://controller.internal:8443`), `PALAI_CONTROLLER_DNS` (the exact SAN in the gateway server
cert — the runner pins it as the TLS ServerName regardless of the dial host, so it may differ from
the URL host), `PALAI_RUNNER_ID`/`PALAI_RUNNER_DNS`, and `PALAI_ENGINE_IMAGE`.

The gateway must be reachable from the runner host. The single-node production overlay keeps it on
the internal compose network (only the TLS edge is published); a split-VM deploy publishes the
gateway port to the runner subnet (firewalled to it). See the local proof below.

## Local proof (Docker-network seam)

`scripts/package/runner/splitvm-proof.sh` stands the control-plane + Postgres + object-store up
WITHOUT an in-stack runner and with the gateway port published, extracts the runner from the
SIGNED package, runs it as a container on a SEPARATE Docker network (reaching the gateway via the
Docker host gateway), and drives one real response to `completed`. It proves the packaged runner
enrolls outbound-only from outside the stack's network and runs a real lease. The real two-VM
network, `systemctl enable --now`, and boot persistence are the operator ceiling (§6).

## Related

- `docs/operations/install.md` — the control-plane / stack leg and `palai-stack.service`.
- `docs/operations/backup-restore.md` — `palai-backup.service` / `.timer`.
