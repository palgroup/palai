"""Server-Sent Events framing + the shared backoff — a port of the TS SDK's ``stream.ts`` framer.

``_SSEDecoder`` is a stateful line framer both the sync and async stream loops feed byte chunks
into, so the WHATWG event-stream parse (blank-line dispatch, comment skip, one-space-after-colon,
multi-line ``data`` joined with ``\\n``, CRLF tolerance) lives in ONE place regardless of transport.
"""

from __future__ import annotations

import codecs
import json
import random
import re
from dataclasses import dataclass
from typing import Any, Iterable, Iterator


@dataclass
class SSEFrame:
    """A parsed SSE frame. ``data`` is the joined data lines; ``id`` updates the reconnection cursor;
    ``event`` is the event name the server set. A frame with neither data, event, nor id (a bare
    heartbeat comment) is never emitted."""

    id: str | None = None
    event: str | None = None
    data: str = ""


@dataclass
class _MutableFrame:
    id: str | None = None
    event: str | None = None
    data: str = ""
    has_data: bool = False


class _SSEDecoder:
    """Incremental SSE line framer. ``feed(chunk)`` returns any frames completed by that chunk;
    ``flush()`` is not needed — a stream ends on a terminal event or transport close, and a partial
    trailing line (no blank line) is intentionally NOT dispatched (matches the TS framer)."""

    def __init__(self) -> None:
        self._buffer = ""
        self._frame = _MutableFrame()
        # An incremental UTF-8 decoder holds a multibyte char split across chunk boundaries until its
        # bytes arrive (the TS framer's TextDecoder(stream:true) equivalent) — a per-chunk decode would
        # turn a split `é`/emoji into U+FFFD and silently deliver a corrupted event under real TCP.
        self._decoder = codecs.getincrementaldecoder("utf-8")("replace")

    def feed(self, chunk: bytes) -> list[SSEFrame]:
        self._buffer += self._decoder.decode(chunk)
        out: list[SSEFrame] = []
        while True:
            newline = self._buffer.find("\n")
            if newline == -1:
                break
            line = self._buffer[:newline]
            if line.endswith("\r"):
                line = line[:-1]
            self._buffer = self._buffer[newline + 1 :]
            if line == "":
                frame = self._frame
                if frame.data != "" or frame.event is not None or frame.id is not None:
                    out.append(SSEFrame(id=frame.id, event=frame.event, data=frame.data))
                self._frame = _MutableFrame()
                continue
            self._apply_field(line)
        return out

    def _apply_field(self, line: str) -> None:
        if line.startswith(":"):
            return  # comment / heartbeat
        colon = line.find(":")
        if colon == -1:
            field_name, value = line, ""
        else:
            field_name, value = line[:colon], line[colon + 1 :]
            if value.startswith(" "):
                value = value[1:]
        if field_name == "id":
            self._frame.id = value
        elif field_name == "event":
            self._frame.event = value
        elif field_name == "data":
            self._frame.data = value if not self._frame.has_data else f"{self._frame.data}\n{value}"
            self._frame.has_data = True
        # retry: and unknown fields are not used by this consumer.


def parse_event_stream(chunks: Iterable[bytes]) -> Iterator[SSEFrame]:
    """Frame a synchronous byte-chunk iterable into SSE frames (the sync counterpart of the TS
    ``parseEventStream``). It buffers a partial trailing line across reads and never buffers the
    whole response — one frame is produced per blank-line dispatch."""
    decoder = _SSEDecoder()
    for chunk in chunks:
        for frame in decoder.feed(chunk):
            yield frame


_TERMINAL_EVENT = re.compile(r"^(run|response)\.(completed|failed|canceled|timed_out|budget_exceeded)\.v[0-9]+$")


def is_terminal_event(event: dict[str, Any]) -> bool:
    """Report whether an event closes the run, so the stream stops rather than reconnecting when the
    server closes a completed stream (§22.3, §24.4)."""
    return isinstance(event.get("type"), str) and bool(_TERMINAL_EVENT.match(event["type"]))


def full_jitter_backoff(attempt: int, base_ms: float, max_ms: float) -> float:
    """The AWS "full jitter" schedule: a delay in ``[0, min(max_ms, base_ms * 2**attempt)]``, which
    spreads retries so many clients do not synchronize their reconnect storms (§23.7). ``attempt`` is
    0-based; a non-positive base disables backoff."""
    if base_ms <= 0 or attempt < 0:
        return 0.0
    ceiling = min(max_ms, base_ms * (2 ** attempt))
    return random.random() * ceiling
