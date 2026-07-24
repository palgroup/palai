"""Webhook signature verify (§21.5, API-014) — the receiver-side helper mirrored byte-for-byte from
the server signer. The known vector below is the SAME one the shared corpus pins."""

from palai import webhook

SECRET = b"secret-key-alpha"
WID = "tcall_conf_001"
TS = 1700000000
BODY = b'{"operation_id":"rop_conf_001","protocol":"tool-http.v1","result":{"answer":"sunny"},"tool_call_id":"tcall_conf_001"}'
SIG = "331c05b40f99e9aeb3e256739c0c0cf4004146216c18f2092eda99602538621d"


def test_sign_matches_known_server_vector():
    assert webhook.sign(SECRET, WID, TS, BODY) == SIG


def test_verify_exact_and_within_window():
    assert webhook.verify(SECRET, WID, TS, BODY, f"v1={SIG}", TS, 300) is True
    assert webhook.verify(SECRET, WID, TS, BODY, f"v1={SIG}", TS + 200, 300) is True


def test_verify_stale_outside_tolerance_both_directions():
    assert webhook.verify(SECRET, WID, TS, BODY, f"v1={SIG}", TS + 600, 300) is False  # expired
    assert webhook.verify(SECRET, WID, TS, BODY, f"v1={SIG}", TS - 600, 300) is False  # future


def test_verify_tampered_body_and_wrong_secret():
    tampered = BODY.replace(b"sunny", b"rainy")
    assert webhook.verify(SECRET, WID, TS, tampered, f"v1={SIG}", TS, 300) is False
    assert webhook.verify(b"secret-key-bravo", WID, TS, BODY, f"v1={SIG}", TS, 300) is False


def test_verify_malformed_header_no_v1():
    assert webhook.verify(SECRET, WID, TS, BODY, "garbage", TS, 300) is False


def test_verify_rotation_accepts_either_active_secret():
    old = webhook.sign(SECRET, WID, TS, BODY)
    new = webhook.sign(b"secret-key-bravo", WID, TS, BODY)
    header = f"v1={old} v1={new}"
    # A receiver on the old secret verifies the overlap header...
    assert webhook.verify(SECRET, WID, TS, BODY, header, TS, 300) is True
    # ...and so does a receiver already advanced to the new secret.
    assert webhook.verify(b"secret-key-bravo", WID, TS, BODY, header, TS, 300) is True
