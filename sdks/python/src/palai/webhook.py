"""Inbound webhook signature verification (§21.5, §23.10) — the receiver-side helper the TS SDK
does not ship, so the Python leg covers the ``signature-verify`` corpus category the TS runner omits.

It mirrors the server's ``adapters/integrations/webhook`` signer byte-for-byte: the signed input is
``"v1." + delivery_id + "." + unix + "."`` followed by the EXACT raw body, HMAC-SHA-256, hex-encoded.
A rotation header carries several space-separated ``v1=`` values so a receiver on either the old or
new secret verifies during the overlap.
"""

from __future__ import annotations

import hashlib
import hmac

_VERSION = "v1"


def sign(secret: bytes, delivery_id: str, timestamp: int, raw_body: bytes) -> str:
    """Compute the hex HMAC-SHA-256 over the signed input (version, delivery id, timestamp, body)."""
    prefix = f"{_VERSION}.{delivery_id}.{timestamp}.".encode()
    return hmac.new(secret, prefix + raw_body, hashlib.sha256).hexdigest()


def verify(
    secret: bytes,
    delivery_id: str,
    timestamp: int,
    raw_body: bytes,
    header: str,
    now: int,
    tolerance_seconds: int,
) -> bool:
    """Recompute the MAC under ``secret`` and compare in constant time, enforcing the configurable
    timestamp tolerance around ``now`` (the replay window). Accepts a header with several ``v1=``
    values (rotation overlap). ``timestamp``/``now`` are unix seconds; ``tolerance_seconds`` bounds
    ``|now - timestamp|``."""
    skew = now - timestamp
    if skew > tolerance_seconds or skew < -tolerance_seconds:
        return False  # outside the replay window
    want = sign(secret, delivery_id, timestamp, raw_body)
    for field in header.split():
        if not field.startswith(_VERSION + "="):
            continue
        value = field[len(_VERSION) + 1 :]
        if hmac.compare_digest(value, want):
            return True
    return False
