package adapter

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"zoiko.io/eventing/signature"
)

// Callback verification errors. Callers map these to a receipt outcome and a generic response.
var (
	ErrCallbackSignature = errors.New("callback signature verification failed")
	ErrCallbackStale     = errors.New("callback is outside the replay window")
	ErrCallbackMalformed = errors.New("callback body is malformed")
	ErrCallbackStatus    = errors.New("callback carries a status the profile does not define")
)

// CallbackEvent is what a verified callback says.
type CallbackEvent struct {
	EventRef   string
	PaymentRef string
	Status     string // the profile's mapped class (ACKNOWLEDGED, PENDING, REJECTED, SETTLED)
	RawStatus  string
	EventTime  *time.Time
}

// CallbackHeaders are the request headers the verifier needs.
type CallbackHeaders struct {
	Signature string
	EventID   string // zoiko-v1 only
}

// VerifyCallback authenticates the raw body against the profile's scheme with the provided secrets (more
// than one while a secret rotates), then reads the event. Order matters: authenticate first, parse second.
func (c Callback) VerifyCallback(h CallbackHeaders, raw []byte, secrets [][]byte, now time.Time) (CallbackEvent, error) {
	switch c.Scheme {
	case "hmac-sha256-hex":
		sig := strings.ToLower(strings.TrimSpace(h.Signature))
		if len(sig) != sha256.Size*2 {
			return CallbackEvent{}, ErrCallbackSignature
		}
		ok := false
		for _, k := range secrets {
			m := hmac.New(sha256.New, k)
			m.Write(raw)
			if hmac.Equal([]byte(hex.EncodeToString(m.Sum(nil))), []byte(sig)) {
				ok = true
			}
		}
		if !ok {
			return CallbackEvent{}, ErrCallbackSignature
		}
	case "zoiko-v1":
		if h.EventID == "" {
			return CallbackEvent{}, ErrCallbackSignature
		}
		err := signature.Verify(h.Signature, h.EventID, raw, secrets, now, time.Duration(c.ToleranceSeconds)*time.Second)
		switch {
		case err == nil:
		case errors.Is(err, signature.ErrStale):
			return CallbackEvent{}, ErrCallbackStale
		default:
			return CallbackEvent{}, ErrCallbackSignature
		}
	default:
		return CallbackEvent{}, ErrCallbackSignature
	}
	return c.Extract(raw)
}

// Extract reads a body without authenticating it (the caller has).
func (c Callback) Extract(raw []byte) (CallbackEvent, error) {
	var doc any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return CallbackEvent{}, ErrCallbackMalformed
	}
	get := func(p string) string {
		v, _ := getPath(doc, p)
		return strings.TrimSpace(asString(v))
	}
	ev := CallbackEvent{EventRef: get(c.EventRefPath), PaymentRef: get(c.PaymentRefPath), RawStatus: get(c.StatusPath)}
	if ev.EventRef == "" || ev.PaymentRef == "" || len(ev.EventRef) > 128 || len(ev.PaymentRef) > 128 {
		return CallbackEvent{}, ErrCallbackMalformed
	}
	mapped, ok := c.StatusMap[ev.RawStatus]
	if !ok {
		return CallbackEvent{}, ErrCallbackStatus
	}
	ev.Status = mapped
	if c.EventTimePath != "" {
		if s := get(c.EventTimePath); s != "" {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				t = t.UTC()
				ev.EventTime = &t
			}
		}
	}
	return ev, nil
}
