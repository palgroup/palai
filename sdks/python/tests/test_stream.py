"""Resumable SSE stream — a mid-run drop reconnects from Last-Event-ID and de-duplicates the
redelivered boundary event; a terminal event stops; final_response resolves the canonical Response."""

import httpx
import pytest

from palai import Palai, PalaiConnectionError

BASE = "http://palai.test"
CREATE = {"id": "resp_1", "session_id": "ses_1", "object": "response", "status": "queued"}


def _client(handler) -> Palai:
    return Palai(api_key="k", base_url=BASE, transport=httpx.MockTransport(handler), backoff_base_ms=1, backoff_max_ms=1)


def test_drop_then_resume_dedupes_boundary_event():
    calls = {"events": 0}

    def handler(request: httpx.Request) -> httpx.Response:
        url = str(request.url)
        if url.endswith("/v1/responses") and request.method == "POST":
            return httpx.Response(202, json=CREATE)
        if "/events" in url:
            calls["events"] += 1
            if calls["events"] == 1:
                # First connection: one progress event, then EOF WITHOUT a terminal → a drop.
                body = 'id: e1\nevent: run.progress.v1\ndata: {"type":"run.progress.v1","id":"e1"}\n\n'
                return httpx.Response(200, headers={"content-type": "text/event-stream"}, content=body.encode())
            assert request.headers.get("Last-Event-ID") == "e1", "reconnect must resume from the cursor"
            # Second connection: the server redelivers the e1 boundary (deduped), then the terminal.
            body = (
                'id: e1\nevent: run.progress.v1\ndata: {"type":"run.progress.v1","id":"e1"}\n\n'
                'id: e2\nevent: run.completed.v1\ndata: {"type":"run.completed.v1","id":"e2"}\n\n'
            )
            return httpx.Response(200, headers={"content-type": "text/event-stream"}, content=body.encode())
        return httpx.Response(200, json={"id": "resp_1", "status": "completed"})  # retrieve

    client = _client(handler)
    stream = client.responses.stream({"input": "hi"})
    ids = [event["id"] for event in stream]
    client.close()
    assert ids == ["e1", "e2"], "e1 delivered once (dedup on resume), e2 terminal stops"
    assert calls["events"] == 2
    assert stream.last_event_id == "e2"


def test_final_response_drains_to_terminal_then_retrieves():
    def handler(request: httpx.Request) -> httpx.Response:
        url = str(request.url)
        if url.endswith("/v1/responses") and request.method == "POST":
            return httpx.Response(202, json=CREATE)
        if "/events" in url:
            body = 'id: e1\nevent: run.completed.v1\ndata: {"type":"run.completed.v1","id":"e1"}\n\n'
            return httpx.Response(200, headers={"content-type": "text/event-stream"}, content=body.encode())
        return httpx.Response(200, json={"id": "resp_1", "status": "completed", "model": "fake-1"})

    client = _client(handler)
    final = client.responses.stream({"input": "hi"}).final_response()
    client.close()
    assert final["id"] == "resp_1"
    assert final["status"] == "completed"


def test_status_error_on_open_is_terminal_not_a_drop():
    def handler(request: httpx.Request) -> httpx.Response:
        url = str(request.url)
        if url.endswith("/v1/responses") and request.method == "POST":
            return httpx.Response(202, json=CREATE)
        return httpx.Response(410, json={"type": "t", "title": "gone", "status": 410, "code": "gone"})

    client = _client(handler)
    from palai import GoneError

    with pytest.raises(GoneError):
        list(client.responses.stream({"input": "hi"}))
    client.close()


def test_double_consumption_is_rejected():
    def handler(request: httpx.Request) -> httpx.Response:
        url = str(request.url)
        if url.endswith("/v1/responses") and request.method == "POST":
            return httpx.Response(202, json=CREATE)
        if "/events" in url:
            body = 'id: e1\nevent: run.completed.v1\ndata: {"type":"run.completed.v1","id":"e1"}\n\n'
            return httpx.Response(200, headers={"content-type": "text/event-stream"}, content=body.encode())
        return httpx.Response(200, json={"id": "resp_1"})

    client = _client(handler)
    stream = client.responses.stream({"input": "hi"})
    list(stream)
    with pytest.raises(PalaiConnectionError):
        list(stream)  # a stream is single-use
    client.close()
