"""The Python leg of the shared tool-sdk conformance corpus (spec §28.23, TOL-018).

TOL-018's Python foot is MINIMAL by decision (plan §8.6, pin 2026-07-22): it runs
the schema-emit and signature-verify families of the SAME corpus the Go/TS legs
run — a verify+schema subset that proves polyglot parity on the security-critical
surfaces. The full Python SDK (result-normalize, idempotency, a server) is E16.
"""

import json
import pathlib

import palai_tool_sdk as sdk

CORPUS = pathlib.Path(__file__).resolve().parents[3] / "tests" / "conformance" / "tool-sdk" / "corpus"


def _load(name):
    return json.loads((CORPUS / name).read_text())["vectors"]


def test_schema_emit_canonical_bytes():
    vectors = _load("schema-emit.json")
    assert vectors, "empty schema-emit corpus"
    for v in vectors:
        assert sdk.define_tool(v["definition"]).decode() == v["canonical"], v["name"]


def test_signature_verify_and_sign_parity():
    vectors = _load("signature-verify.json")
    assert vectors, "empty signature-verify corpus"
    for v in vectors:
        secret = v["secret"].encode()
        body = v["body"].encode()
        got = sdk.verify(
            secret, v["webhook_id"], v["timestamp"], body,
            v["signature"], v["now"], v["tolerance_seconds"],
        )
        assert got == v["expect"], v["name"]
        if v.get("expect_signature"):
            assert sdk.sign(secret, v["webhook_id"], v["timestamp"], body) == v["expect_signature"], v["name"]
