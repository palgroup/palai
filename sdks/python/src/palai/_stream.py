"""Resumable, typed consumers of a run's event stream — a port of the TS SDK's ``ResponseStream``.

Both the sync ``ResponseStream`` and the async ``AsyncResponseStream`` iterate canonical events
(unknown event types are delivered, not dropped: API-009). A transport drop before a terminal event
reconnects from the last seen id via ``Last-Event-ID`` with full-jitter backoff, dropping a single
redelivered boundary event; a terminal event or an exhausted reconnect budget stops. They share the
``_SSEDecoder`` framer and the ``full_jitter_backoff`` schedule; only the sync/async control flow —
which genuinely differs — is written twice.
"""

from __future__ import annotations

import asyncio
import json
import time
from typing import Any, AsyncIterator, Awaitable, Callable, Iterator

from ._sse import _SSEDecoder, full_jitter_backoff, is_terminal_event
from .errors import PalaiConnectionError, error_for_response

StreamStart = dict  # {"response_id": str, "session_id": str}


def _decode_event(data: str) -> dict[str, Any] | None:
    try:
        parsed = json.loads(data)
    except (ValueError, TypeError):
        return None
    if isinstance(parsed, dict) and isinstance(parsed.get("type"), str):
        return parsed
    return None


def _started_from(created: dict[str, Any]) -> StreamStart:
    return {"response_id": str(created["id"]), "session_id": str(created.get("session_id") or "")}


class ResponseStream:
    """A synchronous iterator over a run's events. Iterate it for each event, or call
    ``final_response()`` to drain to the terminal and return the canonical Response."""

    def __init__(
        self,
        transport: Any,
        start: Callable[[], dict[str, Any]],
        *,
        last_event_id: str | None = None,
        max_reconnects: int = 5,
        backoff_base_ms: float = 100,
        backoff_max_ms: float = 5000,
    ) -> None:
        self._transport = transport
        self._start = start
        self._started: StreamStart | None = None
        self._last_event_id = last_event_id
        self._max_reconnects = max_reconnects
        self._backoff_base_ms = backoff_base_ms
        self._backoff_max_ms = backoff_max_ms
        self._consumed = False

    @property
    def response_id(self) -> str | None:
        return self._started["response_id"] if self._started else None

    @property
    def session_id(self) -> str | None:
        return self._started["session_id"] if self._started else None

    @property
    def last_event_id(self) -> str | None:
        return self._last_event_id

    def _ensure_started(self) -> StreamStart:
        if self._started is None:
            self._started = _started_from(self._start())
        return self._started

    def __iter__(self) -> Iterator[dict[str, Any]]:
        if self._consumed:
            raise PalaiConnectionError("this response stream has already been consumed")
        self._consumed = True
        return self._run()

    def final_response(self) -> Any:
        it = iter(self)
        try:
            for event in it:
                if is_terminal_event(event):
                    break
        finally:
            it.close()
        started = self._ensure_started()
        return self._transport.retrieve_response(started["response_id"])

    def _run(self) -> Iterator[dict[str, Any]]:
        start = self._ensure_started()
        reconnects = 0
        while True:
            resumed_from = self._last_event_id
            try:
                resp = self._transport.open_event_stream(start["session_id"], resumed_from)
            except PalaiConnectionError:
                if reconnects >= self._max_reconnects:
                    raise PalaiConnectionError("event stream could not be (re)opened")
                time.sleep(full_jitter_backoff(reconnects, self._backoff_base_ms, self._backoff_max_ms) / 1000)
                reconnects += 1
                continue
            if resp.status_code >= 400:
                body = _safe_read_sync(resp)
                resp.close()
                raise error_for_response(resp.status_code, body, resp.headers.get("Request-Id"))
            decoder = _SSEDecoder()
            dedupe_pending = resumed_from is not None
            dropped = False
            try:
                for chunk in resp.iter_bytes():
                    for frame in decoder.feed(chunk):
                        if frame.id is not None:
                            self._last_event_id = frame.id
                        if frame.data == "":
                            continue
                        event = _decode_event(frame.data)
                        if event is None:
                            continue
                        if dedupe_pending:
                            dedupe_pending = False
                            if frame.id is not None and frame.id == resumed_from:
                                continue  # duplicate of the resume boundary
                        yield event
                        if is_terminal_event(event):
                            return  # terminal reached: stop, do not reconnect
            except (OSError, RuntimeError):
                dropped = True  # a mid-stream transport drop → reconnect below
            finally:
                resp.close()
            # The stream ended (or dropped) without a terminal event: reconnect, bounded, with backoff.
            _ = dropped
            if reconnects >= self._max_reconnects:
                raise PalaiConnectionError("event stream dropped before a terminal event and exhausted reconnects")
            time.sleep(full_jitter_backoff(reconnects, self._backoff_base_ms, self._backoff_max_ms) / 1000)
            reconnects += 1


class AsyncResponseStream:
    """The async counterpart of ``ResponseStream``: ``async for`` each event, or ``await
    stream.final_response()``."""

    def __init__(
        self,
        transport: Any,
        start: Callable[[], Awaitable[dict[str, Any]]],
        *,
        last_event_id: str | None = None,
        max_reconnects: int = 5,
        backoff_base_ms: float = 100,
        backoff_max_ms: float = 5000,
    ) -> None:
        self._transport = transport
        self._start = start
        self._started: StreamStart | None = None
        self._last_event_id = last_event_id
        self._max_reconnects = max_reconnects
        self._backoff_base_ms = backoff_base_ms
        self._backoff_max_ms = backoff_max_ms
        self._consumed = False

    @property
    def response_id(self) -> str | None:
        return self._started["response_id"] if self._started else None

    @property
    def session_id(self) -> str | None:
        return self._started["session_id"] if self._started else None

    @property
    def last_event_id(self) -> str | None:
        return self._last_event_id

    async def _ensure_started(self) -> StreamStart:
        if self._started is None:
            self._started = _started_from(await self._start())
        return self._started

    def __aiter__(self) -> AsyncIterator[dict[str, Any]]:
        if self._consumed:
            raise PalaiConnectionError("this response stream has already been consumed")
        self._consumed = True
        return self._run()

    async def final_response(self) -> Any:
        it = self.__aiter__()
        try:
            async for event in it:
                if is_terminal_event(event):
                    break
        finally:
            await it.aclose()
        started = await self._ensure_started()
        return await self._transport.retrieve_response(started["response_id"])

    async def _run(self) -> AsyncIterator[dict[str, Any]]:
        start = await self._ensure_started()
        reconnects = 0
        while True:
            resumed_from = self._last_event_id
            try:
                resp = await self._transport.open_event_stream(start["session_id"], resumed_from)
            except PalaiConnectionError:
                if reconnects >= self._max_reconnects:
                    raise PalaiConnectionError("event stream could not be (re)opened")
                await asyncio.sleep(full_jitter_backoff(reconnects, self._backoff_base_ms, self._backoff_max_ms) / 1000)
                reconnects += 1
                continue
            if resp.status_code >= 400:
                body = await _safe_read_async(resp)
                await resp.aclose()
                raise error_for_response(resp.status_code, body, resp.headers.get("Request-Id"))
            decoder = _SSEDecoder()
            dedupe_pending = resumed_from is not None
            try:
                async for chunk in resp.aiter_bytes():
                    for frame in decoder.feed(chunk):
                        if frame.id is not None:
                            self._last_event_id = frame.id
                        if frame.data == "":
                            continue
                        event = _decode_event(frame.data)
                        if event is None:
                            continue
                        if dedupe_pending:
                            dedupe_pending = False
                            if frame.id is not None and frame.id == resumed_from:
                                continue
                        yield event
                        if is_terminal_event(event):
                            return
            except (OSError, RuntimeError):
                pass  # a mid-stream transport drop → reconnect below
            finally:
                await resp.aclose()
            if reconnects >= self._max_reconnects:
                raise PalaiConnectionError("event stream dropped before a terminal event and exhausted reconnects")
            await asyncio.sleep(full_jitter_backoff(reconnects, self._backoff_base_ms, self._backoff_max_ms) / 1000)
            reconnects += 1


def _safe_read_sync(resp: Any) -> str:
    try:
        return resp.read().decode("utf-8", errors="replace")
    except Exception:
        return ""


async def _safe_read_async(resp: Any) -> str:
    try:
        return (await resp.aread()).decode("utf-8", errors="replace")
    except Exception:
        return ""
