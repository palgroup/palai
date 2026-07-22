"""Minimal Python leg of the tool-http.v1 Extension SDK (spec §28.23, TOL-018).

TOL-018's Python foot is a MINIMAL verify+schema module by decision (plan §8.6,
pin 2026-07-22): it proves polyglot parity on the two security-critical surfaces —
canonical schema emit and standard-webhooks signature verify+sign — against the
SAME shared corpus the full Go/TS SDKs run. The result-normalize, idempotency, and
server surfaces are the full Go/TS SDKs' job; the full Python SDK is E16.

Standard library only (hashlib/hmac/json), so it drops into a customer tool server
with no dependency. Secrets are in-memory bytes, never logged.
"""

import hashlib
import hmac
import json

SIGNATURE_VERSION = "v1"


def canonical(obj) -> bytes:
    """Sorted-key compact JSON bytes, byte-identical to the Go/TS canonical form."""
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()


def define_tool(definition: dict) -> bytes:
    """Emit the tool revision registration body as canonical bytes.

    Only the known executor-config fields are emitted; unset optionals are dropped
    with the same omit rule as the Go/TS legs (a secret is a secret_ref handle
    only, never a raw credential).
    """
    executor = definition.get("executor")
    input_schema = definition.get("input_schema")
    if not executor:
        raise ValueError("extsdk: tool definition needs an executor")
    if input_schema is None:
        raise ValueError("extsdk: tool definition needs an input_schema")
    body = {"executor": executor, "input_schema": input_schema}
    for key in ("description", "replay_class", "secret_ref"):
        value = definition.get(key)
        if value:
            body[key] = value
    for key in ("output_schema", "executor_config"):
        value = definition.get(key)
        if value is not None:
            body[key] = value
    timeout_ms = definition.get("timeout_ms")
    if timeout_ms is not None:
        body["timeout_ms"] = timeout_ms
    return canonical(body)


def sign(secret: bytes, delivery_id: str, ts: int, body: bytes) -> str:
    """Hex HMAC-SHA-256 over the standard-webhooks signed input (spec §21.5)."""
    message = f"{SIGNATURE_VERSION}.{delivery_id}.{ts}.".encode() + body
    return hmac.new(secret, message, hashlib.sha256).hexdigest()


def verify(
    secret: bytes,
    delivery_id: str,
    ts: int,
    body: bytes,
    header: str,
    now: int,
    tolerance: int,
) -> bool:
    """Constant-time verify of a signed invoke/callback.

    Recomputes the MAC over the raw body, enforces the timestamp tolerance on BOTH
    skew directions (the replay window), and accepts a header carrying several v1=
    values (rotation overlap). Mirrors the Go/TS Verify verbatim.
    """
    skew = now - ts
    if skew > tolerance or skew < -tolerance:
        return False
    want = sign(secret, delivery_id, ts, body)
    for field in header.split():
        if not field.startswith(SIGNATURE_VERSION + "="):
            continue
        value = field[len(SIGNATURE_VERSION) + 1:]
        if hmac.compare_digest(value, want):
            return True
    return False
