"""Typed RFC 9457 error mapping (§23.7) — the family class, code, retryable, and request-id
projection the conformance corpus pins."""

from palai import (
    AuthenticationError,
    ConflictError,
    GoneError,
    InternalServerError,
    InvalidRequestError,
    NotFoundError,
    PermissionDeniedError,
    RateLimitError,
    error_for_response,
    is_retryable_status,
)


def _problem(status: int, code: str, **extra) -> str:
    import json

    return json.dumps({"type": "t", "title": code, "status": status, "code": code, **extra})


def test_family_class_per_status():
    cases = {
        400: InvalidRequestError,
        422: InvalidRequestError,
        401: AuthenticationError,
        403: PermissionDeniedError,
        404: NotFoundError,
        409: ConflictError,
        410: GoneError,
        429: RateLimitError,
        500: InternalServerError,
        503: InternalServerError,
    }
    for status, cls in cases.items():
        err = error_for_response(status, _problem(status, "some_code", request_id="req_x"))
        assert isinstance(err, cls), f"{status} -> {type(err).__name__}, want {cls.__name__}"
        assert err.status == status
        assert err.code == "some_code"
        assert err.request_id == "req_x"


def test_retryable_by_status_class():
    assert error_for_response(429, _problem(429, "rate_limited")).retryable is True
    assert error_for_response(500, _problem(500, "internal_error")).retryable is True
    assert error_for_response(400, _problem(400, "invalid_request")).retryable is False


def test_explicit_retryable_overrides_status_class():
    # A 400 is non-retryable by class, but an explicit problem.retryable=true wins.
    err = error_for_response(400, _problem(400, "invalid_request", retryable=True))
    assert err.retryable is True


def test_non_problem_body_synthesizes_typed_error():
    err = error_for_response(502, "Bad Gateway", "req_502")
    assert isinstance(err, InternalServerError)
    assert err.code == "internal_error"
    assert err.retryable is True
    assert err.request_id == "req_502"


def test_gone_410_retention_purged_is_terminal():
    err = error_for_response(410, _problem(410, "retention_expired", request_id="req_410"))
    assert isinstance(err, GoneError)
    assert err.code == "retention_expired"
    assert err.retryable is False


def test_is_retryable_status_predicate():
    assert is_retryable_status(408) and is_retryable_status(429) and is_retryable_status(503)
    assert not is_retryable_status(400) and not is_retryable_status(404)
