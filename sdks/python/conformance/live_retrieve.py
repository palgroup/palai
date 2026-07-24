"""live_retrieve is the palai (Python) SDK leg of the E16 T8 four-client parity journey.

Given a response id in ``PALAI_LIVE_RESPONSE_ID`` and a live server (``PALAI_BASE_URL`` +
``PALAI_API_KEY``, the SDK's env defaults), it retrieves that SHARED response over the REAL SDK
client and prints its NORMALIZED projection ``{"id","output_text","status"}`` on stdout — the exact
shape the CLI + Go + TypeScript legs emit. The journey canonical-bytes-diffs the four decodes. A 410
tombstone (a purged store:false response) prints a gone marker and exits 3, so the journey can assert
the TYPED gone surface. Driven in the uv-locked env:

    uv run --locked --project sdks/python python sdks/python/conformance/live_retrieve.py
"""
from __future__ import annotations

import json
import os
import sys

from palai import Palai, PalaiAPIError


def main() -> None:
    rid = os.environ.get("PALAI_LIVE_RESPONSE_ID")
    if not rid:
        sys.stderr.write("PALAI_LIVE_RESPONSE_ID is unset\n")
        sys.exit(2)
    client = Palai()  # reads PALAI_BASE_URL + PALAI_API_KEY
    try:
        resp = client.responses.retrieve(rid)
    except PalaiAPIError as exc:
        if getattr(exc, "status", None) == 410:
            sys.stdout.write(json.dumps({"gone": True, "status": 410}))
            sys.exit(3)
        raise
    items = resp.get("output") or []
    text = "".join(it.get("text", "") for it in items if isinstance(it, dict) and isinstance(it.get("text"), str))
    sys.stdout.write(json.dumps({"id": resp.get("id"), "output_text": text, "status": resp.get("status")}))


if __name__ == "__main__":
    main()
