# Palai Release Signing and Promotion Policy

This policy applies to every public Palai runtime, CLI, SDK, protocol bundle,
container image, Helm chart, and offline installation bundle. A development
snapshot is not a release and must not be presented as supported or stable.

## Required release identity

Every release is anchored to one immutable Git commit and one annotated,
cryptographically signed Git tag. The release index records that commit, the
tag verification result, exact build workflow identity, source revision,
toolchain versions, and every published artifact digest.

Artifact names and mutable registry tags are discovery aids only. Installers,
deployment manifests, evidence, and support bundles identify release artifacts
by SHA-256 digest.

## Required release artifacts

The release index includes and checksums:

- control-plane, runner, reference-engine, and migration artifacts;
- CLI and official TypeScript, Python, and Go SDK packages;
- canonical OpenAPI, AsyncAPI, JSON Schema, and protocol bundles;
- SPDX or CycloneDX SBOMs for each binary, image, and package;
- build provenance binding source, workflow, toolchains, and output digests;
- signatures and verification bundles for online and offline verification;
- redacted conformance evidence and the supported compatibility matrix.

Missing artifacts, unverifiable provenance, signature mismatch, mutable-only
references, or evidence checksum mismatch blocks promotion.

## Two-person promotion

Stable and release-candidate promotion is a two-person operation. One
maintainer starts the protected release workflow and a different authorized
maintainer reviews the release index, conformance evidence, dependency and
vulnerability results, then approves the protected environment. The builder
cannot bypass this gate, including as a repository administrator.

Until two maintainers and a protected release environment exist, Palai may
publish development snapshots but must not publish an RC or stable release.

## Signing and key boundary

CI obtains short-lived signing identity only inside the protected release job.
Long-lived private signing keys are not stored in Git, repository variables,
runner images, build logs, artifacts, or operator documentation. Signature
verification is bound to the expected repository, workflow, commit, and issuer;
an identity-valid signature from a different workflow is rejected.

Offline bundles carry the required public trust roots, transparency material or
equivalent verification bundle, revocation metadata, and exact verification
commands. Offline verification never requires a telemetry or license heartbeat.

As implemented (E18 T3/T4), a trust root that TRAVELS WITH the bundle is not a
trust root: an attacker who can swap the artifacts can swap the key beside them,
and the signature becomes a second checksum. So `scripts/release/release-verify.sh`
REFUSES a public key — and a verifying script — that resolves from inside the
release it is checking, and delivery of the root out of band is operator
ceremony (the E14 T5 model). Revocation metadata is likewise read from an
out-of-band list (`PALAI_RELEASE_REVOCATIONS`); `release-index.json` defines its
shape and deliberately carries no pointer to one. The signature is openssl
ECDSA P-256 over a SLSA-SHAPED in-toto attestation: there is no transparency
log entry, no identity service, and no SLSA level to claim.

## Revocation and rebuilds

A compromised signer, workflow, dependency, or artifact triggers publication
freeze, credential revocation, advisory preparation, and a new version. A
released tag or artifact is never overwritten. Rebuilding the same commit is a
new attested build with distinct provenance; it cannot silently replace an
existing digest.

## Implementation gate

E18 implements the pinned signing tools, protected GitHub environment, SBOM and
provenance generation, offline verifier, and two-person release workflow. Until
that gate passes, repository CI protects source integration but does not prove a
signed Palai release.

