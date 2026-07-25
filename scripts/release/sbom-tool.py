#!/usr/bin/env python3
"""scripts/release/sbom-tool.py — E18 T2. The JSON half of scripts/release/sbom.sh.

Three subcommands, one shared policy evaluator:

  gate    <policy> <report.json>...       run the §51.3 vulnerability policy over scanner reports
  rollup                                  (env-driven) build the license inventory + vuln report and
                                          PATCH the release index / SDK manifest with the SBOM slots
  verify                                  (env-driven) re-check a populated dir OFFLINE: every
                                          indexed artifact has both SBOMs, every SBOM digest is
                                          RECOMPUTED from its bytes, and the gate decision is
                                          RE-DERIVED from the raw findings rather than read back

Recompute-over-copy (plan §2) runs through all three: a digest always comes from the file's bytes,
the DB snapshot always comes from the scanner's own `descriptor.db` record, and the gate decision
always comes from the raw match list. Nothing is transcribed from a log or trusted from a manifest.
"""

import collections
import datetime
import hashlib
import json
import os
import sys

MAX_EXCEPTION_DAYS = 90


class Refused(Exception):
    """A fail-closed refusal: printed as `sbom-tool: <msg>` and exits non-zero."""


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def load(path):
    with open(path, encoding="utf-8") as fh:
        return json.load(fh)


def dump(path, doc):
    with open(path, "w", encoding="utf-8") as fh:
        json.dump(doc, fh, indent=2, ensure_ascii=False)
        fh.write("\n")


# --- the §51.3 policy gate ------------------------------------------------------------------------

def _validate_exception(exc, idx):
    """A malformed exception REFUSES the policy file. It is never silently dropped (which would let
    a critical through) and never silently applied (which would make `expires` decorative)."""
    for field in ("id", "owner", "reason", "opened", "expires"):
        if not str(exc.get(field) or "").strip():
            raise Refused("vuln-policy exception #%d has no %s — an exception is only ever "
                          "time-bound and owner-attributed (§62.2 P2)" % (idx, field))
    try:
        opened = datetime.date.fromisoformat(exc["opened"])
        expires = datetime.date.fromisoformat(exc["expires"])
    except ValueError as err:
        raise Refused("vuln-policy exception %s has an unparseable date: %s" % (exc["id"], err))
    if expires <= opened:
        raise Refused("vuln-policy exception %s expires (%s) on or before it opened (%s)"
                      % (exc["id"], exc["expires"], exc["opened"]))
    if (expires - opened).days > MAX_EXCEPTION_DAYS:
        raise Refused("vuln-policy exception %s runs %d days; the ceiling is %d — an open-ended "
                      "exception is a silent acceptance, not a deferral"
                      % (exc["id"], (expires - opened).days, MAX_EXCEPTION_DAYS))
    return opened, expires


def evaluate(policy, reports, on_date):
    """reports: {artifact -> grype report dict}. Returns the decision; never raises for a finding,
    only for a malformed policy (which is an operator error, and fails closed either way)."""
    if policy.get("schema") != "palai.vuln-policy/v1":
        raise Refused("vuln-policy schema is %r, want palai.vuln-policy/v1" % policy.get("schema"))
    blocks = policy.get("blocks")
    if not isinstance(blocks, list) or not blocks:
        raise Refused("vuln-policy names no blocking severity — a gate that blocks nothing is not a gate")
    blocking_set = {str(s).lower() for s in blocks}

    exceptions = policy.get("exceptions", [])
    if not isinstance(exceptions, list):
        raise Refused("vuln-policy exceptions must be a list")
    parsed = []
    for i, exc in enumerate(exceptions):
        _opened, expires = _validate_exception(exc, i)
        parsed.append((exc, expires))

    blocking, excepted, severities = [], [], collections.Counter()
    for artifact in sorted(reports):
        for match in reports[artifact].get("matches", []):
            vuln, art = match["vulnerability"], match["artifact"]
            severity = vuln.get("severity", "Unknown")
            severities[severity] += 1
            if severity.lower() not in blocking_set:
                continue
            finding = {
                "artifact": artifact,
                "id": vuln["id"],
                "severity": severity,
                "package": art.get("name"),
                "version": art.get("version"),
                "purl": art.get("purl"),
                "fix": (vuln.get("fix") or {}).get("state"),
            }
            hit = None
            for exc, expires in parsed:
                if exc["id"] != vuln["id"]:
                    continue
                if exc.get("artifact") and exc["artifact"] != artifact:
                    continue
                if expires < on_date:
                    finding["exception_expired"] = "%s expired on %s" % (exc["id"], exc["expires"])
                    continue
                hit = exc
                break
            if hit is None:
                blocking.append(finding)
            else:
                excepted.append(dict(finding, exception=hit))

    return {
        "schema": "palai.vuln-decision/v1",
        "evaluated_on": on_date.isoformat(),
        "blocking_severities": sorted(blocking_set),
        "max_exception_days": MAX_EXCEPTION_DAYS,
        "findings_by_severity": dict(sorted(severities.items())),
        "findings_total": sum(severities.values()),
        "blocking": blocking,
        "excepted": excepted,
        "result": "blocked" if blocking else "pass",
    }


def print_decision(decision, where=sys.stderr):
    print("sbom-tool: %d finding(s) %s; %d blocking, %d excepted → %s"
          % (decision["findings_total"], decision["findings_by_severity"],
             len(decision["blocking"]), len(decision["excepted"]),
             decision["result"].upper()), file=where)
    for f in decision["blocking"]:
        print("  BLOCK %s %s in %s %s (%s)%s"
              % (f["severity"], f["id"], f["package"], f["version"], f["artifact"],
                 " — " + f["exception_expired"] if f.get("exception_expired") else ""), file=where)
    for f in decision["excepted"]:
        print("  EXCEPTED %s %s (owner %s, expires %s)"
              % (f["severity"], f["id"], f["exception"]["owner"], f["exception"]["expires"]), file=where)


# --- db provenance --------------------------------------------------------------------------------

def db_record(reports, lock):
    """The pinned-snapshot record, RECOMPUTED from what the scanner itself wrote into every report,
    then cross-checked against the lock. Two reports disagreeing means two DBs were used in one run."""
    # grype nests the DB record under descriptor.db.status; older shapes put it flat. Take whichever
    # is there rather than assuming, and let the `valid`/`from` checks below decide.
    def status(report):
        db = report["descriptor"]["db"]
        return db.get("status", db)

    seen = {json.dumps(status(r), sort_keys=True) for r in reports.values()}
    if len(seen) != 1:
        raise Refused("the scanner reports name %d different vulnerability DBs — one release run "
                      "must use ONE pinned snapshot" % len(seen))
    db = json.loads(seen.pop())
    if not db.get("valid", False):
        raise Refused("the scanner reported the vulnerability DB as INVALID")
    if lock["archive_sha256"] not in db.get("from", ""):
        raise Refused("the DB the scan actually used (%s) is not the locked snapshot %s"
                      % (db.get("from"), lock["archive_sha256"]))
    return {
        "snapshot_date": lock["snapshot_date"],
        "built": db.get("built"),
        "schema_version": db.get("schemaVersion"),
        "archive_sha256": lock["archive_sha256"],
        "archive_url": lock["archive_url"],
        "source_recorded_by_scanner": db.get("from"),
        "pinned_by": "scripts/release/vulndb.lock.json",
        "honest_ceiling": (
            "A PINNED OFFLINE SNAPSHOT taken on %s, not a live CVE feed. Every scan ran with "
            "--network none and auto-update off, so a vulnerability published after that date is "
            "invisible to this result. Rescanning already-shipped artifacts against a newer "
            "snapshot is an operator leg (plan §6), not something this release performs."
            % lock["snapshot_date"]),
    }


SCAN_CEILING = (
    "The scan sees EXACTLY what the SBOM sees, because the SBOM is its input: for a static Go "
    "binary that is the Go module list the linker embedded (no OS packages, no C libraries); for a "
    "container image it is that image's package DB; for a source/declaration tree it is what the "
    "go.mod / package.json / requirements declare. Anything copied in without metadata, statically "
    "linked from C, or living in a layer no cataloger can read is outside this result, and a clean "
    "scan does not speak for it."
)

SBOM_BYTES_NOTE = (
    "SBOM bytes are NOT reproducible: syft stamps each document with its own id and a timestamp, so "
    "two runs over identical artifacts yield different SBOM digests. The digests below are of THIS "
    "run's bytes. T1's binary-level reproducibility claim is unaffected — it is a property of the "
    "artifacts, which are hashed independently of their SBOMs."
)


# --- license inventory ----------------------------------------------------------------------------

def _spdx_license(pkg):
    for field in ("licenseConcluded", "licenseDeclared"):
        value = (pkg.get(field) or "").strip()
        if value and value != "NOASSERTION":
            return value
    return "NOASSERTION"


def _purl(pkg):
    for ref in pkg.get("externalRefs", []):
        if ref.get("referenceType") == "purl":
            return ref.get("referenceLocator")
    return None


def license_inventory(entries, sbom_dir):
    """Derived from the SPDX documents' bytes — the canonical source — not from a second syft run."""
    rows, by_license = [], collections.Counter()
    for entry in entries:
        doc = load(os.path.join(sbom_dir, entry["spdx"].split("/")[-1]))
        for pkg in doc.get("packages", []):
            if not pkg.get("versionInfo"):
                continue  # the synthetic "the scanned thing itself" package, not a dependency
            lic = _spdx_license(pkg)
            by_license[lic] += 1
            rows.append({
                "artifact": entry["artifact"],
                "package": pkg.get("name"),
                "version": pkg.get("versionInfo"),
                "purl": _purl(pkg),
                "license": lic,
            })
    return {
        "schema": "palai.license-inventory/v1",
        "derived_from": "the SPDX documents in this directory (licenseConcluded, else licenseDeclared)",
        "packages_total": len(rows),
        "by_license": dict(sorted(by_license.items(), key=lambda kv: (-kv[1], kv[0]))),
        "note": ("NOASSERTION means the cataloger found no license metadata in the artifact — it is "
                 "NOT a claim that the package is unlicensed. Resolving those is a legal-review "
                 "step, not something a scanner can decide."),
        "packages": rows,
    }


# --- subcommands ------------------------------------------------------------------------------------

def cmd_gate(argv):
    if len(argv) < 2:
        raise Refused("usage: sbom-tool.py gate <policy.json> <report.json>...")
    policy = load(argv[0])
    reports = {os.path.basename(p): load(p) for p in argv[1:]}
    decision = evaluate(policy, reports, datetime.date.today())
    print_decision(decision)
    json.dump(decision, sys.stdout, indent=2)
    sys.stdout.write("\n")
    return 1 if decision["result"] == "blocked" else 0


def _entries(tsv_path, sbom_dir, scanned):
    """One SBOM slot per manifest artifact, with every digest recomputed from bytes."""
    out, reports = [], {}
    with open(tsv_path, encoding="utf-8") as fh:
        for line in fh:
            kind, artifact, base = line.rstrip("\n").split("\t")
            slot = {"artifact": artifact, "kind": kind}
            for fmt, suffix in (("spdx", ".spdx.json"), ("cyclonedx", ".cdx.json")):
                path = os.path.join(sbom_dir, base + suffix)
                if not os.path.isfile(path):
                    raise Refused("no %s SBOM for %s at %s" % (fmt, artifact, path))
                slot[fmt] = "sbom/" + base + suffix
                slot[fmt + "_digest"] = "sha256:" + sha256_file(path)
            if scanned:
                path = os.path.join(sbom_dir, base + ".grype.json")
                if not os.path.isfile(path):
                    raise Refused("no scanner report for %s at %s" % (artifact, path))
                slot["scan_report"] = "sbom/" + base + ".grype.json"
                slot["scan_report_digest"] = "sha256:" + sha256_file(path)
                reports[artifact] = load(path)
            out.append(slot)
    if not out:
        raise Refused("no artifacts — an SBOM run over nothing proves nothing")
    return out, reports


def cmd_rollup():
    directory = os.environ["DIR"]
    sbom_dir = os.path.join(directory, "sbom")
    scanned = os.environ["SCANNED"] == "1"
    entries, reports = _entries(os.environ["TSV"], sbom_dir, scanned)

    dump(os.path.join(sbom_dir, "license-inventory.json"), license_inventory(entries, sbom_dir))

    block = {
        "formats": ["spdx-json", "cyclonedx-json"],
        "dir": "sbom",
        "generator": {"image": os.environ["SYFT_IMAGE"], "tool": "syft"},
        "license_inventory": "sbom/license-inventory.json",
        "sbom_bytes_note": SBOM_BYTES_NOTE,
        "coverage_ceiling": SCAN_CEILING,
    }

    if scanned:
        lock = load(os.environ["LOCK"])
        policy = load(os.environ["POLICY"])
        decision = evaluate(policy, reports, datetime.date.today())
        report = {
            "schema": "palai.vuln-report/v1",
            "scanner": {"image": os.environ["GRYPE_IMAGE"], "tool": "grype"},
            "db": db_record(reports, lock),
            "policy": "sbom/vuln-policy.json",
            "coverage_ceiling": SCAN_CEILING,
            "decision": decision,
        }
        dump(os.path.join(sbom_dir, "vuln-report.json"), report)
        block["vulnerability_scan"] = {
            "scanned": True,
            "scanner": report["scanner"],
            "db": report["db"],
            "policy": "sbom/vuln-policy.json",
            "report": "sbom/vuln-report.json",
            "report_digest": "sha256:" + sha256_file(os.path.join(sbom_dir, "vuln-report.json")),
            "result": decision["result"],
            "blocking": len(decision["blocking"]),
            "excepted": len(decision["excepted"]),
        }
        print_decision(decision)
    else:
        block["vulnerability_scan"] = {
            "scanned": False,
            "reason": "--no-scan: this run produced SBOMs only",
            "result": "not-scanned",
            "note": ("NOT a clean result. No vulnerability claim may be made for this directory; a "
                     "release run never passes --no-scan."),
        }

    manifest_path = os.path.join(directory, os.environ["MANIFEST"])
    doc = load(manifest_path)
    key = "packages" if os.environ["SDK"] == "1" else "artifacts"
    by_artifact = {e["artifact"]: e for e in entries}
    for item in doc[key]:
        if item.get("file") in by_artifact:
            slot = {k: v for k, v in by_artifact[item["file"]].items()
                    if k not in ("artifact", "kind")}
            item["sbom"] = slot
    doc["sbom"] = block
    doc.pop("sbom_note", None)  # the E16 T7 "intentionally null" note is retired by real values
    dump(manifest_path, doc)

    print("sbom-tool: %d artifact(s) → %d SBOM files in %s; patched %s"
          % (len(entries), 2 * len(entries), sbom_dir, os.environ["MANIFEST"]), file=sys.stderr)
    if scanned and block["vulnerability_scan"]["result"] == "blocked":
        print("sbom-tool: BLOCKED by the §51.3 critical-vulnerability policy — this release cannot "
              "be promoted until every blocking finding is fixed or carries a live, owner-attributed "
              "exception in scripts/release/vuln-policy.json", file=sys.stderr)
        return 1
    return 0


def cmd_verify():
    directory = os.environ["DIR"]
    sbom_dir = os.path.join(directory, "sbom")
    doc = load(os.path.join(directory, os.environ["MANIFEST"]))
    key = "packages" if os.environ["SDK"] == "1" else "artifacts"
    block = doc.get("sbom")
    if not isinstance(block, dict):
        raise Refused("%s carries no sbom block — the SBOMs were never produced (or were dropped)"
                      % os.environ["MANIFEST"])

    expected, reports = set(), {}
    for item in doc[key]:
        artifact = item.get("file")
        if not artifact:
            continue
        slot = item.get("sbom")
        if not isinstance(slot, dict):
            raise Refused("%s has NO sbom slot — an artifact without an SBOM fails index "
                          "verification" % artifact)
        for fmt in ("spdx", "cyclonedx"):
            rel = slot.get(fmt)
            if not rel:
                raise Refused("%s: the sbom slot names no %s document" % (artifact, fmt))
            path = os.path.join(directory, rel)
            if not os.path.isfile(path):
                raise Refused("%s: %s is missing from the release directory" % (artifact, rel))
            got = "sha256:" + sha256_file(path)
            if got != slot.get(fmt + "_digest"):
                raise Refused("%s: %s is %s but the index says %s — the SBOM bytes changed"
                              % (artifact, rel, got, slot.get(fmt + "_digest")))
            expected.add(os.path.basename(rel))
        rel = slot.get("scan_report")
        if rel:
            path = os.path.join(directory, rel)
            if not os.path.isfile(path):
                raise Refused("%s: scan report %s is missing" % (artifact, rel))
            got = "sha256:" + sha256_file(path)
            if got != slot.get("scan_report_digest"):
                raise Refused("%s: %s is %s but the index says %s"
                              % (artifact, rel, got, slot.get("scan_report_digest")))
            expected.add(os.path.basename(rel))
            reports[artifact] = load(path)

    # No unindexed SBOM may ride along — the sibling of T3's "every file must be in the signed
    # sha256sums" hardening, one level up: every SBOM must belong to an INDEXED artifact.
    known = expected | {"license-inventory.json", "vuln-report.json", "vuln-policy.json"}
    riders = sorted(f for f in os.listdir(sbom_dir) if f not in known)
    if riders:
        raise Refused("sbom/ carries %d file(s) no indexed artifact claims: %s"
                      % (len(riders), ", ".join(riders)))

    scan = block.get("vulnerability_scan") or {}
    if scan.get("scanned"):
        if not reports:
            raise Refused("the index claims a vulnerability scan but no artifact names a report")
        recorded = load(os.path.join(directory, scan["report"]))
        # RE-DERIVE the decision from the raw findings with the COPIED policy, at the date the run
        # recorded. That date lives inside the signed root (T3), so it cannot be backdated without
        # breaking the signature; re-evaluating at `today` instead would make a verified release
        # spontaneously fail the day an exception lapses.
        on_date = datetime.date.fromisoformat(recorded["decision"]["evaluated_on"])
        fresh = evaluate(load(os.path.join(sbom_dir, "vuln-policy.json")), reports, on_date)
        if fresh["result"] != recorded["decision"]["result"]:
            raise Refused("the recorded decision is %r but re-deriving it from the raw findings "
                          "gives %r" % (recorded["decision"]["result"], fresh["result"]))
        if fresh["result"] != scan.get("result"):
            raise Refused("the index says %r, the report says %r"
                          % (scan.get("result"), fresh["result"]))
        if fresh["result"] == "blocked":
            print_decision(fresh)
            raise Refused("this release carries %d blocking finding(s)" % len(fresh["blocking"]))
        db_record(reports, {
            "archive_sha256": scan["db"]["archive_sha256"],
            "archive_url": scan["db"]["archive_url"],
            "snapshot_date": scan["db"]["snapshot_date"],
        })
        stale = [e for e in fresh["excepted"]
                 if datetime.date.fromisoformat(e["exception"]["expires"]) < datetime.date.today()]
        for e in stale:
            print("sbom-tool: NOTE %s was excepted at cut time but that exception expired on %s — "
                  "rescanning shipped artifacts is an operator leg (plan §6)"
                  % (e["id"], e["exception"]["expires"]), file=sys.stderr)
    elif scan.get("result") != "not-scanned":
        raise Refused("the sbom block claims %r without a scan" % scan.get("result"))

    print("sbom-tool: OK — %d artifact(s), %d SBOM file(s) re-hashed, decision %r re-derived from "
          "the raw findings" % (len(doc[key]), len(expected), scan.get("result")), file=sys.stderr)
    return 0


def main():
    try:
        if len(sys.argv) > 1 and sys.argv[1] == "gate":
            return cmd_gate(sys.argv[2:])
        if len(sys.argv) > 1 and sys.argv[1] == "rollup":
            return cmd_rollup()
        if len(sys.argv) > 1 and sys.argv[1] == "verify":
            return cmd_verify()
        raise Refused("usage: sbom-tool.py gate|rollup|verify")
    except Refused as err:
        print("sbom-tool: " + str(err), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
