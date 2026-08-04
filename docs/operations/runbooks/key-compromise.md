# Runbook — key compromise and revocation

**Fires when:** an API key, a runner enrollment token, a provider/integration secret, or a release
signing key may be in the wrong hands. Treat a **signing-key** compromise as P0 and follow
[`../../security/vulnerability-process.md`](../../security/vulnerability-process.md) §5 in parallel —
this runbook is the operator half of that section.

**Pinned transcript:** [`transcripts/key-compromise.txt`](./transcripts/key-compromise.txt)

**Order matters:** mint the replacement **first**, cut over, then revoke. Revoking first locks you out
of the surface you need to mint the replacement with.

## A. API key

```sh
palai apikey list
palai apikey create --project prj_local --scope provision
curl -s -o /dev/null -w "%{http_code}\n" "$BASE/v1/projects" -H "Authorization: Bearer $KEYVAL"
palai apikey revoke "$NEWKEY"
curl -s -o /dev/null -w "%{http_code}\n" "$BASE/v1/projects" -H "Authorization: Bearer $KEYVAL"
palai apikey get "$NEWKEY"
```

The two `curl`s are the point: **200 before the revoke, 401 immediately after, same key**. Revocation
is enforced at the request, not merely recorded — a `revoked_at` timestamp in a JSON body would prove
only that a row changed. `GET /v1/projects` is gated on the `provision` capability the replacement key
is minted with, so a 200 means the credential was accepted and authorised.

> **THESE PROBED `/v1/organizations` UNTIL A.2 TASK 6, AND THAT HAD BROKEN THE CHECK RATHER THAN
> WEAKENED IT.** The route is unmounted, so both curls answered **404** — the same code before and
> after the revoke. The step would have printed an identical number for a live key and a dead one and
> read as "ran fine", which is the failure this runbook exists to rule out. The probe must be a route
> that is BOTH mounted and authenticated; an unmounted one refuses everybody and proves nothing.
>
> The pinned transcript below was recorded against the old route and still shows it. It is left as the
> record of what was run that day rather than edited — but it no longer matches these commands, so it
> must be **re-recorded** on the next stack that runs this runbook.

`create` prints the key value **exactly once**; it is never retrievable again, and the recorder
redacts it out of the transcript while still using it for the two probes above. Give the replacement
the **narrowest scope that works** (`--scope provision`), not the empty scope set that means full
capability. After the revoke, `get` still returns the row with `revoked_at` set: the record is kept,
the credential is dead. Reference: [`../admin-cli.md`](../admin-cli.md).

**Blast radius.** A key is scoped to one project. Enumerate what it could reach with
`palai apikey get`, and list the runs it made from the journal before you decide the incident is over.

## B. Runner enrollment credential

```sh
shasum -a 256 "$PALAI_HOME/runner-token"
palai local up >/dev/null 2>&1
shasum -a 256 "$PALAI_HOME/runner-token"
```

`palai local up` mints a **fresh enrollment token** on every invocation, so re-running it rotates
the runner's enrollment secret; the two digests in the transcript differ. Rotation is what retires a
leaked token — the token is **not** one-use: the runner re-presents it to recover an identity that
expired before renewal could roll it forward (a sleeping host), so a leaked token stays usable until
it is rotated. It is rate-limited to one certificate per issued certificate lifetime, so a leak
mints at most one identity per lifetime, not a fleet. On a production or split-VM install the token
is minted by hand and copied to the runner host — the exact steps are in
[`../runner-host.md`](../runner-host.md) (step 2) and [`../install.md`](../install.md).

A runner that is already connected keeps its short-lived workload identity until it expires. If you
need it **off now**, stop the runner host service; there is no operator `revoke` CLI for a runner
today (the gateway's cordon/drain/revoke primitives are used by `palai upgrade` and by graceful
shutdown — see [`../upgrade.md`](../upgrade.md)). Re-enrollment with the new token brings it back.

## C. Provider and integration secrets

```sh
printf %s throwaway | palai secret create --name incident-demo
```

The transcript pins a **404** here, and that is the point: the secret-ref surface is mounted only where
a master key is configured, i.e. the production overlay. On a local stack it is genuinely not there,
and the CLI says so instead of pretending to rotate something. On a production install:

- rotate with `palai secret rotate <name>` (value on stdin — there is deliberately no `--value` flag),
  per [`../admin-cli.md`](../admin-cli.md) (Secrets);
- then revoke the old credential **at the provider**, which this platform cannot do for you;
- a secret value never enters model context and is redeemed only in the executor (`MCI-002`), so the
  exposure surface is the store and the destination, not the transcript of a run.

**Master-key compromise** is a different, larger event: everything in the secret store must be treated
as exposed. The recovery path is [`../dr-drills.md`](../dr-drills.md) §"Master-key recovery (DR-005)",
and the restored stack's secrets are canary-verified under the target key (`DR-006`).

## D. Release / audit signing key

```sh
openssl ecparam -genkey -name prime256v1 -noout -out "$WORK/wrong.key"
openssl pkey -in "$WORK/wrong.key" -pubout -out "$WORK/oob/wrong.pub"
palai audit verify --checkpoint "$WORK/ir-anchor/audit-checkpoint.json" --pubkey "$WORK/oob/wrong.pub"
```

Verification is **fail-closed against an out-of-band public key**. The transcript shows the wrong key
producing `ALERT [signature]` and a non-zero exit, never a warning that could be scrolled past. The
same openssl P-256 signer verifies release artifacts, so the property is identical there
([`../airgap.md`](../airgap.md) §1).

When the **release** signing identity is compromised, this runbook stops and
[`../../security/release-policy.md`](../../security/release-policy.md) takes over — freeze, revoke,
advisory, new version, never overwrite a released tag or artifact. Then:

1. Re-verify every artifact an operator may hold, offline, so the advisory can state which digests are
   known-good: `scripts/release/release-verify.sh` for a whole release directory (E18 T4 — recomputes
   every artifact digest, the SBOM digests, the provenance binding and the signed root, and runs under
   `--network none`), [`../airgap.md`](../airgap.md) §1 for an air-gap bundle. Both resolve the trust
   root and the verifying code from **outside** what they are checking, and refuse rather than fall
   back to a copy that travelled with the artifacts — which is the whole point when the signer is the
   thing that was compromised.
2. Re-cut the audit anchor under the new key and keep the old public key: a checkpoint signed by the
   compromised key is no longer evidence of anything.
3. Verify the audit chain across the incident window:
   [`audit-integrity-alert.md`](./audit-integrity-alert.md).

## Checklist

- [ ] Replacement credential minted and in use **before** revocation
- [ ] Old credential revoked and the revocation observed (`revoked_at`, or a rejected request)
- [ ] Revoked at the third party too, where the credential was theirs
- [ ] Support bundle captured ([`incident-response.md`](./incident-response.md) §3)
- [ ] Audit chain verified across the window
- [ ] Advisory decided ([`../../security/vulnerability-process.md`](../../security/vulnerability-process.md) §4)
- [ ] Row added to [`../known-gaps-1.0.md`](../known-gaps-1.0.md) if anything is left open

## Honest ceiling

There is **no incident search by credential fingerprint** — you cannot ask "where was this key used?"
in one query; you read the journal. There is no automatic propagation of a revocation to a third-party
provider. And the signing identity here is an openssl P-256 key held by the operator, **not** a
transparency-log-backed identity: a compromised key has no public revocation channel beyond the
advisory. All three are recorded in [`../known-gaps-1.0.md`](../known-gaps-1.0.md).
