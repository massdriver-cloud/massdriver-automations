package slack

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"
)

// SignatureTolerance is how far a request timestamp may be from now. It limits replay of a captured request.
const SignatureTolerance = 5 * time.Minute

// SigningSecretFromEnv returns SLACK_SIGNING_SECRET, the signing secret of the Slack app.
func SigningSecretFromEnv() ([]byte, error) {
	s := os.Getenv("SLACK_SIGNING_SECRET")
	if s == "" {
		return nil, errors.New("missing required env vars: [SLACK_SIGNING_SECRET]")
	}
	return []byte(s), nil
}

// Verify checks the signature that Slack puts on interactivity and Events API requests: HMAC-SHA256 of
// "v0:<timestamp>:<body>", hex-encoded.
func Verify(secret []byte, h http.Header, body []byte, now time.Time) bool {
	ts, sig := h.Get("X-Slack-Request-Timestamp"), h.Get("X-Slack-Signature")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if sig == "" || err != nil {
		return false
	}
	if d := now.Sub(time.Unix(sec, 0)); d > SignatureTolerance || d < -SignatureTolerance {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(Sign(secret, ts, body)))
}

// Sign returns the X-Slack-Signature value for body at timestamp ts.
func Sign(secret []byte, ts string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("v0:" + ts + ":"))
	mac.Write(body)
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

// SignRequest sets the Slack signature headers on req. For tests.
func SignRequest(req *http.Request, secret []byte, body []byte, now time.Time) {
	ts := strconv.FormatInt(now.Unix(), 10)
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", Sign(secret, ts, body))
}
