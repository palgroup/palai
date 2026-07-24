"""The transport clients: Bearer auth, the dated ``API-Version`` header, idempotent retry with a
single retry owner (no hidden provider-SDK retry), typed RFC 9457 errors, and the SSE open used by
the streaming layer. ``Palai`` is synchronous (``httpx.Client``); ``AsyncPalai`` is its async twin
(``httpx.AsyncClient``). Both wire the SAME resource groups.

The retry DECISIONS live once (``_BaseClient`` helpers + ``full_jitter_backoff``); the retry LOOP is
written twice because sync ``time.sleep`` and async ``asyncio.sleep`` are genuinely different control
flow — unifying them with a generator would be the kind of cleverness someone decodes at 3am.
"""

from __future__ import annotations

import asyncio
import math
import os
import time
from typing import Any, Awaitable, Callable

import httpx

from ._sse import full_jitter_backoff
from ._stream import AsyncResponseStream, ResponseStream
from .errors import PalaiConnectionError, error_for_response, is_retryable_status
from .resources._common import enc
from .resources._resources import (
    Agents,
    ApiKeys,
    Artifacts,
    MCPConnections,
    ModelRoutes,
    Organizations,
    Projects,
    RepositoryBindings,
    Responses,
    Sessions,
    SecretRefs,
    Tools,
    Triggers,
)

# APIVersion is the dated contract this SDK speaks; it rides every request (§20.13).
APIVersion = "2026-07-16"

_DEFAULT_BASE_URL = "http://localhost:8080"


def _env(name: str) -> str | None:
    value = os.environ.get(name)
    return value.strip() if value and value.strip() else None


class _BaseClient:
    """Shared config + the pure retry decisions both clients use."""

    def __init__(
        self,
        *,
        api_key: str | None,
        base_url: str | None,
        max_retries: int,
        timeout_ms: float,
        backoff_base_ms: float,
        backoff_max_ms: float,
    ) -> None:
        # Precedence: explicit option, then the Palai-scoped environment. No other provider's
        # environment is ever read — the SDK never silently picks up an unrelated key.
        key = api_key or _env("PALAI_API_KEY")
        if not key:
            raise ValueError(
                "palai: an API key is required — pass api_key=... or set PALAI_API_KEY. "
                "Keep it server-side; never expose it to the browser."
            )
        self._api_key = key
        base = base_url or _env("PALAI_BASE_URL") or _DEFAULT_BASE_URL
        self.base_url = base[:-1] if base.endswith("/") else base
        self.max_retries = max_retries
        self.timeout_ms = timeout_ms
        self.backoff_base_ms = backoff_base_ms
        self.backoff_max_ms = backoff_max_ms

    def _headers(self, accept: str, idempotency_key: str | None) -> dict[str, str]:
        headers = {
            "Authorization": f"Bearer {self._api_key}",
            "API-Version": APIVersion,
            "Accept": accept,
        }
        if idempotency_key is not None:
            headers["Idempotency-Key"] = idempotency_key
        return headers

    @staticmethod
    def _is_idempotent(method: str, idempotency_key: str | None, idempotent: bool | None) -> bool:
        # A network-level failure may have already committed on the server, so a request is only
        # re-sent when it is idempotent: a safe method, an Idempotency-Key create, or a body-idempotent
        # op that opts in. A non-idempotent create is NOT re-sent, so a torn connection cannot double
        # provision. Response-level (429/5xx) retry is unaffected — that is pre-commit-safe.
        return method in ("GET", "HEAD") or idempotency_key is not None or idempotent is True

    def _within_budget(self, deadline: float, attempt: int) -> bool:
        return time.monotonic() + full_jitter_backoff(attempt, self.backoff_base_ms, self.backoff_max_ms) / 1000 < deadline

    def _retryable_wait_ms(self, resp: httpx.Response, attempt: int) -> float:
        header = resp.headers.get("Retry-After")
        if header is not None:
            try:
                seconds = float(header)
            except ValueError:
                seconds = -1
            # A non-finite Retry-After (inf/nan) is ignored, so we fall back to jittered backoff rather
            # than wait forever / raise on the deadline check (TS parity: Number.isFinite).
            if math.isfinite(seconds) and seconds >= 0:
                return seconds * 1000
        return full_jitter_backoff(attempt, self.backoff_base_ms, self.backoff_max_ms)


def _parse_body(resp: httpx.Response) -> Any:
    return None if not resp.content else resp.json()


class Palai(_BaseClient):
    """The synchronous Palai client. Every resource hangs off it; ``close()`` (or ``with``) releases
    the underlying connection pool."""

    def __init__(
        self,
        *,
        api_key: str | None = None,
        base_url: str | None = None,
        max_retries: int = 2,
        timeout_ms: float = 60_000,
        backoff_base_ms: float = 200,
        backoff_max_ms: float = 10_000,
        transport: httpx.BaseTransport | None = None,
    ) -> None:
        super().__init__(
            api_key=api_key,
            base_url=base_url,
            max_retries=max_retries,
            timeout_ms=timeout_ms,
            backoff_base_ms=backoff_base_ms,
            backoff_max_ms=backoff_max_ms,
        )
        self._http = httpx.Client(transport=transport) if transport is not None else httpx.Client()
        self.responses = Responses(self)
        self.sessions = Sessions(self)
        self.agents = Agents(self)
        self.artifacts = Artifacts(self)
        self.repository_bindings = RepositoryBindings(self)
        self.tools = Tools(self)
        self.mcp_connections = MCPConnections(self)
        self.triggers = Triggers(self)
        self.secret_refs = SecretRefs(self)
        self.model_routes = ModelRoutes(self)
        self.organizations = Organizations(self)
        self.projects = Projects(self)
        self.api_keys = ApiKeys(self)

    def request(
        self,
        method: str,
        path: str,
        *,
        body: Any = None,
        idempotency_key: str | None = None,
        idempotent: bool | None = None,
        accept: str = "application/json",
        max_retries: int | None = None,
        timeout_ms: float | None = None,
    ) -> Any:
        url = self.base_url + path
        headers = self._headers(accept, idempotency_key)
        idempotent = self._is_idempotent(method, idempotency_key, idempotent)
        retries = self.max_retries if max_retries is None else max_retries
        deadline = time.monotonic() + (self.timeout_ms if timeout_ms is None else timeout_ms) / 1000
        attempt = 0
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise PalaiConnectionError(f"{method} {path} exceeded its deadline")
            try:
                resp = self._http.request(method, url, headers=headers, json=body, timeout=remaining)
            except httpx.HTTPError as cause:
                if not idempotent or attempt >= retries or not self._within_budget(deadline, attempt):
                    raise PalaiConnectionError(f"{method} {path} failed to reach the server") from cause
                time.sleep(full_jitter_backoff(attempt, self.backoff_base_ms, self.backoff_max_ms) / 1000)
                attempt += 1
                continue
            if resp.is_success:
                return _parse_body(resp)
            if is_retryable_status(resp.status_code) and attempt < retries:
                wait = self._retryable_wait_ms(resp, attempt)
                if time.monotonic() + wait / 1000 < deadline:
                    resp.close()
                    time.sleep(wait / 1000)
                    attempt += 1
                    continue
            raise error_for_response(resp.status_code, resp.text, resp.headers.get("Request-Id"))

    def open_event_stream(self, session_id: str, last_event_id: str | None) -> httpx.Response:
        headers = self._headers("text/event-stream", None)
        if last_event_id is not None:
            headers["Last-Event-ID"] = last_event_id
        url = f"{self.base_url}/v1/sessions/{enc(session_id)}/events"
        # No READ timeout on the event stream: a live run may idle past httpx's 5s default between SSE
        # bytes (TS parity — fetch has no read timeout). Connect/write/pool stay bounded.
        req = self._http.build_request("GET", url, headers=headers, timeout=httpx.Timeout(self.timeout_ms / 1000, read=None))
        try:
            return self._http.send(req, stream=True)
        except httpx.HTTPError as cause:
            raise PalaiConnectionError(f"GET {url} failed to reach the server") from cause

    def open_download(self, path: str, *, timeout_ms: float | None = None) -> httpx.Response:
        headers = self._headers("application/octet-stream", None)
        req = self._http.build_request(
            "GET", self.base_url + path, headers=headers,
            timeout=(self.timeout_ms if timeout_ms is None else timeout_ms) / 1000,
        )
        try:
            resp = self._http.send(req, stream=True)
        except httpx.HTTPError as cause:
            raise PalaiConnectionError(f"GET {path} failed to reach the server") from cause
        if not resp.is_success:
            body = resp.read().decode("utf-8", errors="replace")
            resp.close()
            raise error_for_response(resp.status_code, body, resp.headers.get("Request-Id"))
        return resp

    def retrieve_response(self, response_id: str) -> Any:
        return self.responses.retrieve(response_id)

    def _new_response_stream(self, start: Callable[[], dict], *, last_event_id: str | None) -> ResponseStream:
        return ResponseStream(self, start, last_event_id=last_event_id)

    def close(self) -> None:
        self._http.close()

    def __enter__(self) -> "Palai":
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()


class AsyncPalai(_BaseClient):
    """The asynchronous Palai client — the async twin of ``Palai``. ``await``-able resource methods;
    ``await aclose()`` (or ``async with``) releases the pool."""

    def __init__(
        self,
        *,
        api_key: str | None = None,
        base_url: str | None = None,
        max_retries: int = 2,
        timeout_ms: float = 60_000,
        backoff_base_ms: float = 200,
        backoff_max_ms: float = 10_000,
        transport: httpx.AsyncBaseTransport | None = None,
    ) -> None:
        super().__init__(
            api_key=api_key,
            base_url=base_url,
            max_retries=max_retries,
            timeout_ms=timeout_ms,
            backoff_base_ms=backoff_base_ms,
            backoff_max_ms=backoff_max_ms,
        )
        self._http = httpx.AsyncClient(transport=transport) if transport is not None else httpx.AsyncClient()
        self.responses = Responses(self)
        self.sessions = Sessions(self)
        self.agents = Agents(self)
        self.artifacts = Artifacts(self)
        self.repository_bindings = RepositoryBindings(self)
        self.tools = Tools(self)
        self.mcp_connections = MCPConnections(self)
        self.triggers = Triggers(self)
        self.secret_refs = SecretRefs(self)
        self.model_routes = ModelRoutes(self)
        self.organizations = Organizations(self)
        self.projects = Projects(self)
        self.api_keys = ApiKeys(self)

    async def request(
        self,
        method: str,
        path: str,
        *,
        body: Any = None,
        idempotency_key: str | None = None,
        idempotent: bool | None = None,
        accept: str = "application/json",
        max_retries: int | None = None,
        timeout_ms: float | None = None,
    ) -> Any:
        url = self.base_url + path
        headers = self._headers(accept, idempotency_key)
        idempotent = self._is_idempotent(method, idempotency_key, idempotent)
        retries = self.max_retries if max_retries is None else max_retries
        deadline = time.monotonic() + (self.timeout_ms if timeout_ms is None else timeout_ms) / 1000
        attempt = 0
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise PalaiConnectionError(f"{method} {path} exceeded its deadline")
            try:
                resp = await self._http.request(method, url, headers=headers, json=body, timeout=remaining)
            except httpx.HTTPError as cause:
                if not idempotent or attempt >= retries or not self._within_budget(deadline, attempt):
                    raise PalaiConnectionError(f"{method} {path} failed to reach the server") from cause
                await asyncio.sleep(full_jitter_backoff(attempt, self.backoff_base_ms, self.backoff_max_ms) / 1000)
                attempt += 1
                continue
            if resp.is_success:
                return _parse_body(resp)
            if is_retryable_status(resp.status_code) and attempt < retries:
                wait = self._retryable_wait_ms(resp, attempt)
                if time.monotonic() + wait / 1000 < deadline:
                    await resp.aclose()
                    await asyncio.sleep(wait / 1000)
                    attempt += 1
                    continue
            raise error_for_response(resp.status_code, resp.text, resp.headers.get("Request-Id"))

    async def open_event_stream(self, session_id: str, last_event_id: str | None) -> httpx.Response:
        headers = self._headers("text/event-stream", None)
        if last_event_id is not None:
            headers["Last-Event-ID"] = last_event_id
        url = f"{self.base_url}/v1/sessions/{enc(session_id)}/events"
        # No READ timeout on the event stream: a live run may idle past httpx's 5s default between SSE
        # bytes (TS parity — fetch has no read timeout). Connect/write/pool stay bounded.
        req = self._http.build_request("GET", url, headers=headers, timeout=httpx.Timeout(self.timeout_ms / 1000, read=None))
        try:
            return await self._http.send(req, stream=True)
        except httpx.HTTPError as cause:
            raise PalaiConnectionError(f"GET {url} failed to reach the server") from cause

    async def open_download(self, path: str, *, timeout_ms: float | None = None) -> httpx.Response:
        headers = self._headers("application/octet-stream", None)
        req = self._http.build_request(
            "GET", self.base_url + path, headers=headers,
            timeout=(self.timeout_ms if timeout_ms is None else timeout_ms) / 1000,
        )
        try:
            resp = await self._http.send(req, stream=True)
        except httpx.HTTPError as cause:
            raise PalaiConnectionError(f"GET {path} failed to reach the server") from cause
        if not resp.is_success:
            body = (await resp.aread()).decode("utf-8", errors="replace")
            await resp.aclose()
            raise error_for_response(resp.status_code, body, resp.headers.get("Request-Id"))
        return resp

    async def retrieve_response(self, response_id: str) -> Any:
        return await self.responses.retrieve(response_id)

    def _new_response_stream(self, start: Callable[[], Awaitable[dict]], *, last_event_id: str | None) -> AsyncResponseStream:
        return AsyncResponseStream(self, start, last_event_id=last_event_id)

    async def aclose(self) -> None:
        await self._http.aclose()

    async def __aenter__(self) -> "AsyncPalai":
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.aclose()
