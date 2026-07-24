"""Plumbing every resource shares: id path-encoding, the opaque-cursor list query, and the
idempotency-handle minting — the Python counterpart of the TS SDK's ``resources/shared.ts``.

The two page envelopes (``Page`` cursor-paginated vs ``ListView`` full-set admin) are plain dicts
at runtime — the server sends ``{"data":[...], "has_more":...}`` or ``{"object":"list","data":[...]}``
and the SDK hands them back verbatim (unknown fields preserved); nothing here strips or reshapes them.
"""

from __future__ import annotations

import uuid
from typing import Any, Mapping
from urllib.parse import quote, urlencode

# encodeURIComponent leaves the unreserved set plus !*'() unencoded; match it so a path segment
# encodes identically to the TS SDK (the corpus pins ``resp_abc/def`` -> ``resp_abc%2Fdef``).
_ENC_SAFE = "!*'()"


def enc(segment: str) -> str:
    """Percent-encode one path segment exactly as the TS SDK's ``encodeURIComponent`` does."""
    return quote(segment, safe=_ENC_SAFE)


def list_path(path: str, params: Mapping[str, Any] | None = None) -> str:
    """Append the shared list query in the SAME fixed key order the TS ``listPath`` emits
    (after, limit, status, created_after, created_before), so the wire path is byte-identical.
    Absent params add no key; camelCased inputs snake-case on the wire."""
    params = params or {}
    pairs: list[tuple[str, str]] = []
    after = params.get("after")
    if after is not None:
        pairs.append(("after", str(after)))
    limit = params.get("limit")
    if limit is not None:
        pairs.append(("limit", str(limit)))
    status = params.get("status")
    if status is not None:
        pairs.append(("status", str(status)))
    created_after = params.get("created_after", params.get("createdAfter"))
    if created_after is not None:
        pairs.append(("created_after", str(created_after)))
    created_before = params.get("created_before", params.get("createdBefore"))
    if created_before is not None:
        pairs.append(("created_before", str(created_before)))
    if not pairs:
        return path
    return f"{path}?{urlencode(pairs, quote_via=quote)}"


def new_idempotency_key() -> str:
    """Mint a fresh, collision-resistant key for one logical create (§20.9, §35.3)."""
    return f"idem_{uuid.uuid4()}"


def new_command_id() -> str:
    """Mint a stable command idempotency handle for one steer/interrupt (§22.4)."""
    return f"cmd_{uuid.uuid4()}"
