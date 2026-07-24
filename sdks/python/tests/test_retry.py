"""Idempotent retry (API-013) — the SAME Idempotency-Key rides every retry of a retryable failure,
so the server settles exactly one mutation; a non-retryable status is terminal; a persistent network
error retries to the ceiling then raises a typed connection error."""

import httpx
import pytest

from palai import InvalidRequestError, Palai, PalaiConnectionError
from palai._sse import full_jitter_backoff

QUEUED = {"id": "resp_1", "object": "response", "status": "queued", "session_id": "ses_1", "run_id": "run_1"}


def _client(handler) -> Palai:
    return Palai(
        api_key="sk-test",
        base_url="http://palai.test",
        transport=httpx.MockTransport(handler),
        max_retries=3,
        backoff_base_ms=1,
        backoff_max_ms=2,
    )


def test_retryable_failure_retried_with_same_idempotency_key():
    keys: list[str | None] = []
    state = {"n": 0}

    def handler(request: httpx.Request) -> httpx.Response:
        state["n"] += 1
        keys.append(request.headers.get("Idempotency-Key"))
        if state["n"] <= 2:
            return httpx.Response(503, json={"type": "t", "title": "x", "status": 503, "code": "capacity_unavailable"})
        return httpx.Response(202, json=QUEUED)

    client = _client(handler)
    try:
        response = client.responses.create({"input": "hi"})
    finally:
        client.close()

    assert response["id"] == "resp_1"
    assert len(keys) == 3, "two 503s then a success is three attempts"
    assert len(set(keys)) == 1, "the idempotency key must be identical across retries"
    assert keys[0] and keys[0].startswith("idem_")


def test_non_retryable_status_thrown_immediately():
    state = {"n": 0}

    def handler(request: httpx.Request) -> httpx.Response:
        state["n"] += 1
        return httpx.Response(400, json={"type": "t", "title": "x", "status": 400, "code": "invalid_request"})

    client = _client(handler)
    try:
        with pytest.raises(InvalidRequestError):
            client.responses.retrieve("x")
    finally:
        client.close()
    assert state["n"] == 1, "a 400 must not be retried"


def test_persistent_network_error_retries_to_ceiling_then_connection_error():
    state = {"n": 0}

    def handler(request: httpx.Request) -> httpx.Response:
        state["n"] += 1
        raise httpx.ConnectError("network unreachable")

    client = Palai(
        api_key="sk-test", base_url="http://palai.test", transport=httpx.MockTransport(handler),
        max_retries=2, backoff_base_ms=1, backoff_max_ms=2,
    )
    try:
        with pytest.raises(PalaiConnectionError):
            client.responses.retrieve("x")  # GET is safe → idempotent → retried
    finally:
        client.close()
    assert state["n"] == 3, "the initial attempt plus two retries"


def test_non_idempotent_create_not_resent_on_network_drop():
    # A create with a caller-supplied key IS idempotent and would retry; without any key AND no
    # idempotency handle a bare non-idempotent POST fails closed. The SDK mints a key for create, so
    # to prove the fail-closed path we drive a raw POST with no key via the transport method.
    state = {"n": 0}

    def handler(request: httpx.Request) -> httpx.Response:
        state["n"] += 1
        raise httpx.ConnectError("drop after commit?")

    client = _client(handler)
    try:
        with pytest.raises(PalaiConnectionError):
            client.request("POST", "/v1/things")  # no key, not idempotent → not re-sent
    finally:
        client.close()
    assert state["n"] == 1, "a non-idempotent POST must not be re-sent after a network drop"


def test_full_jitter_backoff_bounds():
    base, mx = 100, 5000
    for attempt in range(8):
        ceiling = min(mx, base * (2 ** attempt))
        for _ in range(200):
            w = full_jitter_backoff(attempt, base, mx)
            assert 0 <= w <= ceiling
    assert full_jitter_backoff(20, base, mx) <= mx
    assert full_jitter_backoff(3, 0, mx) == 0
