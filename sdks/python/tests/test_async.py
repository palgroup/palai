"""The async client — same transport semantics (idempotent retry, resumable stream) as the sync
client. Driven via asyncio.run so no pytest-asyncio dependency is needed (one HTTP library, one test
runner — nothing extra)."""

import asyncio

import httpx

from palai import AsyncPalai

BASE = "http://palai.test"
CREATE = {"id": "resp_1", "session_id": "ses_1", "object": "response", "status": "queued"}


def test_async_create_returns_body():
    async def run():
        captured: dict = {}

        def handler(request: httpx.Request) -> httpx.Response:
            captured["idem"] = request.headers.get("Idempotency-Key")
            return httpx.Response(202, json=CREATE)

        async with AsyncPalai(api_key="k", base_url=BASE, transport=httpx.MockTransport(handler)) as client:
            body = await client.responses.create({"input": "hi"})
        assert body["id"] == "resp_1"
        assert captured["idem"].startswith("idem_")

    asyncio.run(run())


def test_async_stream_final_response():
    async def run():
        def handler(request: httpx.Request) -> httpx.Response:
            url = str(request.url)
            if url.endswith("/v1/responses") and request.method == "POST":
                return httpx.Response(202, json=CREATE)
            if "/events" in url:
                body = 'id: e1\nevent: run.completed.v1\ndata: {"type":"run.completed.v1","id":"e1"}\n\n'
                return httpx.Response(200, headers={"content-type": "text/event-stream"}, content=body.encode())
            return httpx.Response(200, json={"id": "resp_1", "status": "completed"})

        async with AsyncPalai(api_key="k", base_url=BASE, transport=httpx.MockTransport(handler)) as client:
            final = await client.responses.stream({"input": "hi"}).final_response()
        assert final["id"] == "resp_1"

    asyncio.run(run())


def test_async_stream_iterates_events():
    async def run():
        def handler(request: httpx.Request) -> httpx.Response:
            url = str(request.url)
            if url.endswith("/v1/responses") and request.method == "POST":
                return httpx.Response(202, json=CREATE)
            body = (
                'id: e1\nevent: run.progress.v1\ndata: {"type":"run.progress.v1","id":"e1"}\n\n'
                'id: e2\nevent: run.completed.v1\ndata: {"type":"run.completed.v1","id":"e2"}\n\n'
            )
            return httpx.Response(200, headers={"content-type": "text/event-stream"}, content=body.encode())

        async with AsyncPalai(api_key="k", base_url=BASE, transport=httpx.MockTransport(handler)) as client:
            ids = [event["id"] async for event in client.responses.stream({"input": "hi"})]
        assert ids == ["e1", "e2"]

    asyncio.run(run())


def test_async_retryable_failure_same_idempotency_key():
    async def run():
        keys: list[str | None] = []
        state = {"n": 0}

        def handler(request: httpx.Request) -> httpx.Response:
            state["n"] += 1
            keys.append(request.headers.get("Idempotency-Key"))
            if state["n"] <= 2:
                return httpx.Response(503, json={"type": "t", "title": "x", "status": 503, "code": "capacity_unavailable"})
            return httpx.Response(202, json=CREATE)

        async with AsyncPalai(
            api_key="k", base_url=BASE, transport=httpx.MockTransport(handler),
            max_retries=3, backoff_base_ms=1, backoff_max_ms=2,
        ) as client:
            body = await client.responses.create({"input": "hi"})
        assert body["id"] == "resp_1"
        assert len(keys) == 3 and len(set(keys)) == 1

    asyncio.run(run())
