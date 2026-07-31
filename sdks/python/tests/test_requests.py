"""Wire request encoding — the method/path/idempotency/body the SDK emits, plus unknown-field
round-trip on decode. These mirror the shared corpus's request-encode + unknown-field vectors at the
unit level (the corpus proves cross-language equality; these prove the Python surface in isolation)."""

import json

import httpx

from palai import Palai

BASE = "http://palai.test"
STUB = {
    "id": "resp_stub",
    "session_id": "sess_stub",
    "object": "response",
    "status": "completed",
    "model": "fake-1",
    "output": [],
    "usage": {"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
    "x_future": "kept",
}
TERMINAL_SSE = (
    'id: e1\nevent: run.completed.v1\ndata: {"type":"run.completed.v1","id":"e1"}\n\n'
).encode()


def _capture():
    captured: dict = {}

    def handler(request: httpx.Request) -> httpx.Response:
        if not captured:
            captured["method"] = request.method
            captured["path"] = str(request.url)[len(BASE):]
            captured["idempotency_key"] = request.headers.get("Idempotency-Key")
            captured["body"] = json.loads(request.content) if request.content else None
        if "/events" in str(request.url):
            return httpx.Response(200, headers={"content-type": "text/event-stream"}, content=TERMINAL_SSE)
        if request.method == "GET":
            return httpx.Response(200, json=STUB)
        return httpx.Response(202, json=STUB)

    return captured, handler


def test_create_rich_body_and_fixed_idempotency_key():
    captured, handler = _capture()
    client = Palai(api_key="k", base_url=BASE, transport=httpx.MockTransport(handler))
    args = {
        "input": "Summarize the onboarding guide in three bullets.",
        "model": "fake-1",
        "instructions": "Answer concisely.",
        "metadata": {"trace": "conf-req-001"},
        "max_output_tokens": 256,
        "tool_choice": "auto",
    }
    client.responses.create(args, idempotency_key="idem_conf_fixed")
    client.close()
    assert captured["method"] == "POST"
    assert captured["path"] == "/v1/responses"
    assert captured["idempotency_key"] == "idem_conf_fixed"
    assert captured["body"] == args  # verbatim, no fields added


def test_create_mints_idempotency_key_when_absent():
    captured, handler = _capture()
    client = Palai(api_key="k", base_url=BASE, transport=httpx.MockTransport(handler))
    client.responses.create({"input": "hi"})
    client.close()
    assert captured["idempotency_key"].startswith("idem_")


def test_stream_shares_create_post():
    captured, handler = _capture()
    client = Palai(api_key="k", base_url=BASE, transport=httpx.MockTransport(handler))
    client.responses.stream({"input": "Stream a haiku.", "model": "fake-1"}, idempotency_key="idem_stream_fixed").final_response()
    client.close()
    # The FIRST captured request is the create POST — stream adds no body fields for streaming.
    assert captured["method"] == "POST"
    assert captured["path"] == "/v1/responses"
    assert captured["idempotency_key"] == "idem_stream_fixed"
    assert captured["body"] == {"input": "Stream a haiku.", "model": "fake-1"}


def test_list_query_snake_case_fixed_order():
    captured, handler = _capture()
    client = Palai(api_key="k", base_url=BASE, transport=httpx.MockTransport(handler))
    client.responses.list({"after": "cursor_abc", "limit": 20, "createdAfter": "2026-07-01"})
    client.close()
    assert captured["method"] == "GET"
    assert captured["path"] == "/v1/responses?after=cursor_abc&limit=20&created_after=2026-07-01"


def test_retrieve_id_path_encoding():
    captured, handler = _capture()
    client = Palai(api_key="k", base_url=BASE, transport=httpx.MockTransport(handler))
    client.responses.retrieve("resp_abc/def")
    client.close()
    assert captured["path"] == "/v1/responses/resp_abc%2Fdef"


def test_unknown_field_preserved_on_decode():
    _, handler = _capture()
    client = Palai(api_key="k", base_url=BASE, transport=httpx.MockTransport(handler))
    got = client.responses.retrieve("resp_stub")
    client.close()
    assert got["x_future"] == "kept"  # an unknown field survives the round-trip (open-world stance)


def test_model_routes_read_back_listview():
    def handler(request: httpx.Request) -> httpx.Response:
        assert request.method == "GET"
        assert str(request.url).endswith("/v1/model-connections")
        return httpx.Response(200, json={"object": "list", "data": [{"id": "mc_1", "provider": "provider-one"}]})

    client = Palai(api_key="k", base_url=BASE, transport=httpx.MockTransport(handler))
    view = client.model_routes.list_connections()
    client.close()
    assert view["object"] == "list"
    assert view["data"][0]["id"] == "mc_1"


def test_session_create_carries_the_label_and_stays_bodyless_without_one():
    """The label must REACH the wire, and the bodyless create must stay byte-identical.

    A resource method that accepts a name and quietly drops it looks exactly like one that works, from
    the caller's side and from the status code. And an SDK that started posting ``{}`` on the
    pre-existing bodyless create would change every request on the wire while breaking nothing visibly.
    """
    captured, handler = _capture()
    client = Palai(api_key="k", base_url=BASE, transport=httpx.MockTransport(handler))
    client.sessions.create(name="Gece Doğrulama")
    assert captured["method"] == "POST"
    assert captured["path"] == "/v1/sessions"
    assert captured["body"] == {"name": "Gece Doğrulama"}

    captured2, handler2 = _capture()
    client2 = Palai(api_key="k", base_url=BASE, transport=httpx.MockTransport(handler2))
    client2.sessions.create()
    assert captured2["body"] is None


def test_session_rename_patches_the_label_and_an_empty_name_reaches_the_wire():
    """An empty label is a REAL value that clears the operator name.

    Dropping it as falsy is the one way this call quietly does nothing while reporting success.
    """
    captured, handler = _capture()
    client = Palai(api_key="k", base_url=BASE, transport=httpx.MockTransport(handler))
    client.sessions.rename("ses_1", "Gece Doğrulama")
    assert captured["method"] == "PATCH"
    assert captured["path"] == "/v1/sessions/ses_1"
    assert captured["body"] == {"name": "Gece Doğrulama"}

    captured2, handler2 = _capture()
    client2 = Palai(api_key="k", base_url=BASE, transport=httpx.MockTransport(handler2))
    client2.sessions.rename("ses_1", "")
    assert captured2["body"] == {"name": ""}
