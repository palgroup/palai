package palai

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// The receiver-side webhook helper (spec §21.5/§23.10): a Go server that receives Palai's
// tool-call callbacks verifies the HMAC-SHA-256 signature over the EXACT raw body, enforcing the
// timestamp replay window and accepting a rotation-overlap header that carries several v1= values.
// This is the SAME routine the server's outbound signer computes, mirrored SDK-side and stdlib-only
// so a caller adds no dependency. It is a server-side helper by design; there is no browser path.
//
// The TS SDK ships no webhook verify (browser-relay stance), so the shared conformance corpus's
// signature-verify category had only the reference implementation until this Go leg — the Go SDK is
// the first shipped SDK to give that category a second independent impl (plan §T4 "signature verify").

const webhookSignatureVersion = "v1"

// SignWebhook computes the hex HMAC-SHA-256 over the signed input (version, delivery id, timestamp,
// then the exact raw body, joined by "."), matching the server's signer. Binding the version and id
// defeats cross-context replay.
func SignWebhook(secret []byte, deliveryID string, ts time.Time, rawBody []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(webhookSignatureVersion + "." + deliveryID + "." + strconv.FormatInt(ts.Unix(), 10) + "."))
	mac.Write(rawBody)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyWebhook is the receiver-side check: it recomputes the MAC over the raw body under secret,
// compares in constant time, and enforces the timestamp tolerance around now (the replay window).
// It accepts a header carrying several space-separated v1= values, so a receiver still on the old
// secret verifies a rotation-overlap attempt.
func VerifyWebhook(secret []byte, deliveryID string, ts time.Time, rawBody []byte, header string, now time.Time, tolerance time.Duration) bool {
	if skew := now.Sub(ts); skew > tolerance || skew < -tolerance {
		return false // outside the replay window
	}
	want := []byte(SignWebhook(secret, deliveryID, ts, rawBody))
	for _, field := range strings.Fields(header) {
		value, ok := strings.CutPrefix(field, webhookSignatureVersion+"=")
		if !ok {
			continue
		}
		if hmac.Equal([]byte(value), want) {
			return true
		}
	}
	return false
}
