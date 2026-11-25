package automation

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// SignatureTolerance is how far a webhook timestamp may be from now. It limits replay of a captured request.
const SignatureTolerance = 5 * time.Minute

// SigningSecretFromEnv decodes MASSDRIVER_SIGNING_SECRET, the `whsec_` secret of the event destination.
func SigningSecretFromEnv() ([]byte, error) {
	return DecodeSigningSecret(os.Getenv("MASSDRIVER_SIGNING_SECRET"))
}

// DecodeSigningSecret decodes a `whsec_` signing secret.
func DecodeSigningSecret(s string) ([]byte, error) {
	if s == "" {
		return nil, fmt.Errorf("missing required env vars: [MASSDRIVER_SIGNING_SECRET]")
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, "whsec_"))
	if err != nil {
		return nil, fmt.Errorf("MASSDRIVER_SIGNING_SECRET is not a whsec_ secret: %w", err)
	}
	return b, nil
}

// Verify checks a Standard Webhooks signature: base64 HMAC-SHA256 of "<id>.<timestamp>.<body>".
func Verify(secret []byte, h http.Header, body []byte, now time.Time) bool {
	id, ts, sigs := h.Get("webhook-id"), h.Get("webhook-timestamp"), h.Get("webhook-signature")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if id == "" || sigs == "" || err != nil {
		return false
	}
	if d := now.Sub(time.Unix(sec, 0)); d > SignatureTolerance || d < -SignatureTolerance {
		return false
	}

	expected := Sign(secret, id, ts, body)
	for _, sig := range strings.Fields(sigs) {
		if v, ok := strings.CutPrefix(sig, "v1,"); ok && hmac.Equal([]byte(v), []byte(expected)) {
			return true
		}
	}
	return false
}

// Sign returns the base64 signature that Massdriver sends after "v1,".
func Sign(secret []byte, id, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(id + "." + timestamp + "."))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// SignRequest sets the webhook headers on req as Massdriver would. For tests.
func SignRequest(req *http.Request, secret []byte, id string, body []byte, now time.Time) {
	ts := strconv.FormatInt(now.Unix(), 10)
	req.Header.Set("webhook-id", id)
	req.Header.Set("webhook-timestamp", ts)
	req.Header.Set("webhook-signature", "v1,"+Sign(secret, id, ts, body))
}
