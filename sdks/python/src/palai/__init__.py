"""Palai control-plane SDK (sync + async).

A thin, typed Python client for the Palai control-plane HTTP API. It speaks the dated ``/v1``
contract (the ``API-Version`` header rides every request), maps failures to typed RFC 9457
problem errors, retries idempotently, and streams a run's events over resumable SSE.

Server-side credential stance (see README): the API key is server-side only. Python is a
server-side language; there is no browser story, by design.
"""

from . import webhook
from ._client import APIVersion, AsyncPalai, Palai
from ._sse import SSEFrame, full_jitter_backoff, is_terminal_event, parse_event_stream
from ._stream import AsyncResponseStream, ResponseStream, StreamStart
from .errors import (
    AuthenticationError,
    ConflictError,
    GoneError,
    InternalServerError,
    InvalidRequestError,
    NotFoundError,
    PalaiAPIError,
    PalaiConnectionError,
    PalaiError,
    PermissionDeniedError,
    RateLimitError,
    error_for_response,
    is_retryable_status,
)

__all__ = [
    "APIVersion",
    "Palai",
    "AsyncPalai",
    "webhook",
    "ResponseStream",
    "AsyncResponseStream",
    "StreamStart",
    "SSEFrame",
    "parse_event_stream",
    "is_terminal_event",
    "full_jitter_backoff",
    "PalaiError",
    "PalaiConnectionError",
    "PalaiAPIError",
    "InvalidRequestError",
    "AuthenticationError",
    "PermissionDeniedError",
    "NotFoundError",
    "ConflictError",
    "GoneError",
    "RateLimitError",
    "InternalServerError",
    "error_for_response",
    "is_retryable_status",
]
