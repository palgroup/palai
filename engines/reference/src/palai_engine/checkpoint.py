"""Reference-kernel checkpoint envelope (spec §26.1-26.2, §26.5).

The engine's checkpoint is the OPAQUE model/tool/context loop state (spec §26.1): the
control plane stores and checksums the bytes but never interprets them (§26.2). This module
owns the wire envelope — deterministic, typed JSON, NOT pickle — so the same loop state
always addresses to the same content checksum, and a restore reconstructs it exactly. The
loop owns capturing/restoring its own fields (Loop.capture_state / Loop.restore_state); this
module only encodes them and builds the checkpoint.offer data.
"""

from __future__ import annotations

import base64
import json

FORMAT = "reference-kernel"
FORMAT_VERSION = 1
# The "<format>/<version>" token engine.ready.checkpoint_formats advertises and a control-plane
# compatibility check pins against (spec §26.4). One id, so drift can't creep in between the
# advertised list and the checkpoints this engine actually writes.
FORMAT_ID = f"{FORMAT}/{FORMAT_VERSION}"


def encode(state: dict) -> bytes:
    """Canonical JSON bytes for a captured loop state: sorted keys, compact separators, so
    identical state produces byte-identical output (spec §26.2 content-addressing)."""
    return json.dumps(state, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()


def decode(raw: bytes) -> dict:
    """Parse checkpoint bytes back into the captured loop state."""
    return json.loads(raw.decode())


def offer_data(state: dict, boundary_kind: str) -> dict:
    """The checkpoint.offer frame's data (spec §26.2). `state` is base64 of the opaque
    canonical bytes; format/format_version let the control plane pin compatibility and the
    boundary_kind records why the offer was made (tool completion, pause, explicit request)."""
    return {
        "format": FORMAT,
        "format_version": FORMAT_VERSION,
        "boundary_kind": boundary_kind,
        "state": base64.b64encode(encode(state)).decode(),
    }
