// Package webhook sends signed invoice events to contractors' servers
// (ADR-038, invariant I13). Events are created in the same transaction as
// the invoice change that causes them, delivered by the outbox worker at
// least once, retried with backoff for about a day, and every attempt is
// logged in webhook_deliveries.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SignatureHeader carries `t=<unix seconds>,v1=<hex HMAC-SHA256>`.
const SignatureHeader = "Waybill-Signature"

// Tolerance is how far a signature's timestamp may be from the receiver's
// clock. Older signatures are rejected, so a captured request cannot be
// replayed later.
const Tolerance = 5 * time.Minute

// ErrBadSignature covers every verification failure. Receivers should not
// tell a caller which check failed.
var ErrBadSignature = errors.New("webhook: bad signature")

// Sign returns the header value for body sent at t.
func Sign(secret string, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	return "t=" + ts + ",v1=" + mac(secret, ts, body)
}

func mac(secret, ts string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts))
	m.Write([]byte{'.'})
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// Verify checks a header against the raw body, as a receiver should: before
// parsing the body, in constant time, and within Tolerance of now. Any v1
// value may match, which lets a sender sign with an old and a new secret
// during a rotation.
func Verify(secret, header string, body []byte, now time.Time) error {
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return ErrBadSignature
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			sigs = append(sigs, v)
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(sigs) == 0 {
		return ErrBadSignature
	}
	if d := now.Sub(time.Unix(sec, 0)); d > Tolerance || d < -Tolerance {
		return fmt.Errorf("%w: timestamp outside tolerance", ErrBadSignature)
	}
	want := []byte(mac(secret, ts, body))
	for _, s := range sigs {
		if hmac.Equal([]byte(s), want) {
			return nil
		}
	}
	return ErrBadSignature
}
