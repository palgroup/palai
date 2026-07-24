"""Typed RFC 9457 problem errors — a precise port of the TS SDK's ``errors.ts`` (§23.7).

The class names are load-bearing: the conformance corpus pins the error CLASS per status, so
``InvalidRequestError``/``GoneError``/… must be spelled exactly as the TS SDK spells them.
"""

from __future__ import annotations

import json
from typing import Any


class PalaiError(Exception):
    """Base for everything the SDK raises, so a caller can catch the whole surface at once."""


class PalaiConnectionError(PalaiError):
    """A transport failure before any HTTP status was seen — a dropped socket, DNS failure, or a
    per-attempt timeout. It is always retryable."""

    retryable = True


class PalaiAPIError(PalaiError):
    """A typed RFC 9457 problem response: the parsed problem, the HTTP status, the stable code, the
    correlation request id, and whether the class of failure is retryable."""

    def __init__(self, status: int, problem: dict[str, Any], request_id: str | None = None) -> None:
        message = problem.get("detail") or problem.get("title") or problem.get("code") or f"HTTP {status}"
        super().__init__(message)
        self.status = status
        self.problem = problem
        self.code = problem.get("code", "")
        self.request_id = problem.get("request_id") or request_id
        # The server's explicit retryable wins; otherwise fall back to the status class. An explicit
        # ``false`` stays false — only an ABSENT field defers to the status.
        explicit = problem.get("retryable")
        self.retryable = explicit if explicit is not None else is_retryable_status(status)


# Family subclasses give ergonomic isinstance discrimination for the common HTTP classes without a
# class per stable code; the exact code is always on ``.code``.
class InvalidRequestError(PalaiAPIError):
    pass


class AuthenticationError(PalaiAPIError):
    pass


class PermissionDeniedError(PalaiAPIError):
    pass


class NotFoundError(PalaiAPIError):
    pass


class ConflictError(PalaiAPIError):
    pass


class GoneError(PalaiAPIError):
    pass


class RateLimitError(PalaiAPIError):
    pass


class InternalServerError(PalaiAPIError):
    pass


def is_retryable_status(status: int) -> bool:
    """The SDK's default retry predicate (§23.7): a request timeout, a rate limit, or any 5xx."""
    return status == 408 or status == 429 or status >= 500


def _api_error_class(status: int) -> type[PalaiAPIError]:
    return {
        400: InvalidRequestError,
        422: InvalidRequestError,
        401: AuthenticationError,
        403: PermissionDeniedError,
        404: NotFoundError,
        409: ConflictError,
        410: GoneError,
        429: RateLimitError,
    }.get(status, InternalServerError if status >= 500 else PalaiAPIError)


def error_for_response(status: int, body_text: str, request_id: str | None = None) -> PalaiAPIError:
    """Build the typed error for a non-2xx response. A well-formed ``application/problem+json`` body
    is parsed into a problem; a body that is missing or not a problem document degrades to a
    synthesized problem carrying the stable code the status implies, so a gateway's plain-text 502
    still raises a typed, retryable error."""
    problem = _parse_problem(body_text)
    if problem is None:
        problem = _synthetic_problem(status, request_id)
    return _api_error_class(status)(status, problem, request_id)


def _parse_problem(body_text: str) -> dict[str, Any] | None:
    if not body_text:
        return None
    try:
        parsed = json.loads(body_text)
    except (ValueError, TypeError):
        return None
    if not isinstance(parsed, dict):
        return None
    if not isinstance(parsed.get("code"), str) or not isinstance(parsed.get("status"), (int, float)) or isinstance(
        parsed.get("status"), bool
    ):
        return None
    return parsed


def _synthetic_problem(status: int, request_id: str | None) -> dict[str, Any]:
    code = _status_code(status)
    return {
        "type": f"https://docs.palai.dev/problems/{code}",
        "title": code,
        "status": status,
        "code": code,
        "request_id": request_id or "",
    }


def _status_code(status: int) -> str:
    return {
        401: "authentication_required",
        403: "permission_denied",
        404: "not_found",
        409: "active_run_conflict",
        410: "gone",
        429: "rate_limited",
        503: "capacity_unavailable",
        504: "operation_timed_out",
    }.get(status, "internal_error" if status >= 500 else "invalid_request")
