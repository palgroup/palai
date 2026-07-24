"""SDK-conformance runner (E16 T3): the Python leg of the shared, language-agnostic fixture corpus
under ``tests/conformance/sdk/``. It is NOT a test — it is a filter the Go harness drives: it reads
``{"vectors":[{category,name,input}]}`` on stdin, runs each vector through the REAL ``palai`` SDK
surface, and writes ``{"outputs":[{category,name,output}]}`` on stdout as NORMALIZED JSON. The harness
canonical-bytes-diffs that output against the corpus's expected output (and, in T4, against the Go
runner). This is the STABLE runner contract (README.md); the corpus is untouched.

Unlike the TS runner, this covers ALL SIX categories: the Python SDK ships ``palai.webhook.verify``,
so ``signature-verify`` (API-014) is exercised here rather than left to the reference decode alone.
"""

from __future__ import annotations

import json
import sys
from typing import Any

import httpx

from palai import Palai, error_for_response, is_terminal_event, parse_event_stream, webhook

BASE = "http://localhost:8080"

_STUB_RESPONSE = {
    "id": "resp_stub",
    "session_id": "sess_stub",
    "object": "response",
    "status": "completed",
    "model": "fake-1",
    "output": [],
    "usage": {"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
    "created_at": "2026-07-18T00:00:00Z",
}
_TERMINAL_SSE = (
    "id: e1\nevent: run.completed.v1\n"
    'data: {"specversion":"1.0","id":"e1","source":"palai","type":"run.completed.v1",'
    '"time":"2026-07-18T00:00:00Z","sequence":1,"data":{}}\n\n'
).encode()


# --- request-encode: drive the REAL client transport through a capturing MockTransport -----------


def _request_encode(inp: dict[str, Any]) -> Any:
    if inp.get("resource") != "responses":
        return _UNSUPPORTED
    captured: dict[str, Any] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        if not captured:
            captured["method"] = request.method
            captured["url"] = str(request.url)
            captured["idempotency_key"] = request.headers.get("Idempotency-Key")
            captured["body"] = request.content or None
        # stream() opens the session SSE after the create POST: answer with a single terminal frame
        # so final_response() resolves and the run ends cleanly.
        if "/events" in str(request.url):
            return httpx.Response(200, headers={"content-type": "text/event-stream"}, content=_TERMINAL_SSE)
        if request.method == "GET":  # retrieve / list read-backs
            return httpx.Response(200, json=_STUB_RESPONSE)
        return httpx.Response(202, json=_STUB_RESPONSE)  # the create POST

    client = Palai(api_key="conf", base_url=BASE, transport=httpx.MockTransport(handler))
    method = inp.get("method")
    args = inp.get("args") or {}
    options = inp.get("options") or {}
    idem = options.get("idempotencyKey")
    try:
        if method == "create":
            client.responses.create(args, idempotency_key=idem)
        elif method == "stream":
            client.responses.stream(args, idempotency_key=idem).final_response()
        elif method == "list":
            client.responses.list(args)
        elif method == "retrieve":
            client.responses.retrieve(args["id"])
        else:
            return _UNSUPPORTED
    finally:
        client.close()

    if not captured:
        raise RuntimeError(f"request-encode: no request captured for {method}")
    url = captured["url"]
    path = url[len(BASE):] if url.startswith(BASE) else url
    out: dict[str, Any] = {"method": captured["method"], "path": path}
    if captured["idempotency_key"] is not None:
        out["idempotency_key"] = captured["idempotency_key"]
    if captured["body"] is not None:
        out["body"] = json.loads(captured["body"])
    return out


# --- event-decode: frame the SSE transcript through the SDK parser --------------------------------


def _event_decode(inp: dict[str, Any]) -> Any:
    transcript: str = inp["transcript"]
    events: list[Any] = []
    terminal_index = -1
    for frame in parse_event_stream([transcript.encode()]):
        if frame.data == "":
            continue
        try:
            parsed = json.loads(frame.data)
        except (ValueError, TypeError):
            continue
        if not isinstance(parsed, dict) or not isinstance(parsed.get("type"), str):
            continue
        if terminal_index == -1 and is_terminal_event(parsed):
            terminal_index = len(events)
        events.append(parsed)
    return {"events": events, "terminal_index": terminal_index}


# --- error-map: project a wire (status, body) to the typed error surface --------------------------


def _error_map(inp: dict[str, Any]) -> Any:
    err = error_for_response(inp["status"], inp.get("body", ""), inp.get("request_id"))
    return {
        "class": type(err).__name__,
        "status": err.status,
        "code": err.code,
        "retryable": err.retryable,
        "request_id": err.request_id or "",
    }


# --- signature-verify: the webhook helper the TS SDK does not ship --------------------------------


def _signature_verify(inp: dict[str, Any]) -> Any:
    valid = webhook.verify(
        inp["secret"].encode(),
        inp["webhook_id"],
        inp["timestamp"],
        inp["body"].encode(),
        inp["signature"],
        inp["now"],
        inp["tolerance_seconds"],
    )
    out: dict[str, Any] = {"valid": valid}
    if inp.get("expect_signature"):
        out["signature"] = webhook.sign(inp["secret"].encode(), inp["webhook_id"], inp["timestamp"], inp["body"].encode())
    return out


# --- unknown-field / envelope-decode: dict projections (unknown fields survive) -------------------


def _unknown_field(inp: dict[str, Any]) -> Any:
    # Route the value through the REAL client decode (request -> _parse_body -> resp.json), so this
    # category exercises the SDK's forward-compat pipeline rather than echoing the input verbatim.
    value = inp["value"]
    client = Palai(api_key="conf", base_url=BASE, transport=httpx.MockTransport(lambda req: httpx.Response(200, json=value)))
    try:
        return client.request("GET", "/v1/echo")
    finally:
        client.close()


def _envelope_decode(inp: dict[str, Any]) -> Any:
    env = inp["envelope"]
    if "has_more" in env:
        out: dict[str, Any] = {"kind": "page", "has_more": env["has_more"], "data": env["data"]}
        if isinstance(env.get("next_cursor"), str):
            out["next_cursor"] = env["next_cursor"]
        if isinstance(env.get("previous_cursor"), str):
            out["previous_cursor"] = env["previous_cursor"]
        return out
    if env.get("object") == "list":
        return {"kind": "list", "object": env["object"], "data": env["data"]}
    return _UNSUPPORTED


_UNSUPPORTED = object()

_DECODERS = {
    "request-encode": _request_encode,
    "event-decode": _event_decode,
    "error-map": _error_map,
    "signature-verify": _signature_verify,
    "unknown-field": _unknown_field,
    "envelope-decode": _envelope_decode,
}


def main() -> None:
    request = json.loads(sys.stdin.read())
    outputs = []
    for v in request["vectors"]:
        decoder = _DECODERS.get(v["category"])
        if decoder is None:
            continue
        output = decoder(v["input"])
        if output is _UNSUPPORTED:
            continue
        outputs.append({"category": v["category"], "name": v["name"], "output": output})
    sys.stdout.write(json.dumps({"outputs": outputs}))


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:  # noqa: BLE001 — a runner error must exit non-zero with a message on stderr
        sys.stderr.write(f"py-runner: {exc}\n")
        sys.exit(1)
