"""SSE framing (§22.3) — comment skip, CRLF tolerance, multi-line data join, chunk-boundary
resilience, and terminal detection."""

import json

from palai import is_terminal_event, parse_event_stream


def _events(transcript: str, *, chunk: int | None = None) -> list[dict]:
    raw = transcript.encode()
    chunks = [raw] if chunk is None else [raw[i : i + chunk] for i in range(0, len(raw), chunk)]
    out = []
    for frame in parse_event_stream(chunks):
        if frame.data == "":
            continue
        try:
            out.append(json.loads(frame.data))  # a consumer skips a non-JSON data line
        except json.JSONDecodeError:
            continue
    return out


def test_comment_and_crlf_two_events():
    transcript = (
        ": keep-alive\n"
        'id: e1\nevent: model_step.created.v1\ndata: {"type":"model_step.created.v1","id":"e1"}\n\n'
        'id: e2\r\nevent: run.completed.v1\r\ndata: {"type":"run.completed.v1","id":"e2"}\r\n\r\n'
    )
    events = _events(transcript)
    assert [e["id"] for e in events] == ["e1", "e2"]
    assert is_terminal_event(events[1]) and not is_terminal_event(events[0])


def test_multiline_data_joined_with_newline():
    transcript = 'data: {"type":"run.progress.v1",\ndata: "note":"multi"}\n\n'
    events = _events(transcript)
    assert events == [{"type": "run.progress.v1", "note": "multi"}]


def test_nonjson_data_and_heartbeat_skipped():
    transcript = ": ping\ndata: plain text\n\ndata: {\"type\":\"run.progress.v1\"}\n\n"
    events = _events(transcript)
    # "plain text" parses as no JSON object; the framer still yields the frame, the consumer skips it.
    parsed = [e for e in events if isinstance(e, dict)]
    assert parsed == [{"type": "run.progress.v1"}]


def test_split_across_chunk_boundaries_is_stable():
    transcript = 'id: e1\nevent: run.completed.v1\ndata: {"type":"run.completed.v1","id":"e1"}\n\n'
    for chunk in (1, 3, 7, 13):
        events = _events(transcript, chunk=chunk)
        assert events == [{"type": "run.completed.v1", "id": "e1"}], f"chunk={chunk}"


def test_partial_trailing_frame_not_dispatched():
    # No blank line after the data → the frame is buffered, never dispatched (matches the TS framer).
    transcript = 'data: {"type":"run.progress.v1"}\n'
    assert _events(transcript) == []


def test_multibyte_split_across_chunks_not_corrupted():
    # Non-ASCII streamed output (Turkish text, CJK, emoji) split across TCP chunk boundaries must NOT
    # become U+FFFD. chunk=1 splits every multibyte char; a per-chunk decode corrupts, the incremental
    # decoder does not.
    text = "café ğşü 🚀 日本語"
    transcript = 'data: {"type":"run.progress.v1","text":"' + text + '"}\n\n'
    for chunk in (1, 2, 3, 5):
        events = _events(transcript, chunk=chunk)
        assert events == [{"type": "run.progress.v1", "text": text}], f"chunk={chunk}"
    assert "�" not in json.dumps(_events(transcript, chunk=1))


def test_terminal_event_regex_families():
    for t in ("run.completed.v1", "response.failed.v2", "run.budget_exceeded.v1", "response.canceled.v1"):
        assert is_terminal_event({"type": t})
    for t in ("run.progress.v1", "model_step.created.v1", "run.completed", "run.completed.vX"):
        assert not is_terminal_event({"type": t})
