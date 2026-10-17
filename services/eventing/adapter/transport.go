package adapter

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Disposition says how far a call got. It is the most important fact about an outbound call to a bank,
// because it decides whether sending again can duplicate a payment.
type Disposition string

const (
	// NotSent: the request provably never left (refused by policy, DNS, connect or TLS failure). Safe to retry.
	NotSent Disposition = "NOT_SENT"
	// Answered: the provider answered with an HTTP response.
	Answered Disposition = "ANSWERED"
	// Unknown: request bytes may have reached the provider and no answer arrived (timeout, reset). NEVER retry
	// blindly: ask the provider what happened first.
	Unknown Disposition = "UNKNOWN"
)

// Class is the meaning of an answer.
type Class string

const (
	ClassAcknowledged Class = "ACKNOWLEDGED" // the provider took the request (NOT finality)
	ClassPending      Class = "PENDING"      // the provider is still working
	ClassAccepted     Class = "ACCEPTED"     // TAX/EINVOICE only: the authority or network accepted the document
	ClassRejected     Class = "REJECTED"     // the provider refused it for a business reason
	ClassRateLimited  Class = "RATE_LIMITED" // not processed; try later
	ClassNotFound     Class = "NOT_FOUND"    // query only: the provider has no record (proof of non-receipt)
	ClassUnavailable  Class = "UNAVAILABLE"  // 5xx or an unreadable answer: the outcome of the work is unknown
	ClassAuth         Class = "AUTH_FAILED"  // 401/403: credentials, not the payment
	ClassUnparseable  Class = "UNPARSEABLE"  // 2xx we cannot read: the outcome is unknown
	ClassMismatch     Class = "MISMATCH"     // 2xx whose receipt does not echo the reference we sent
)

// Result is what one call produced. Nothing in it is a secret.
type Result struct {
	Disposition  Disposition
	Class        Class
	HTTPStatus   int
	ProviderRef  string
	ProviderStat string
	Reason       string
	RetryAfter   time.Duration
	RequestHash  string
	ResponseHash string
	Duration     time.Duration
	ErrCode      string // a short stable code for transport errors; never provider text
}

// Credential is a short-lived credential used for exactly one call. It must not be stored or logged.
type Credential struct {
	Secret []byte
}

// Call is one request to make.
type Call struct {
	Spec           Spec
	BaseURL        string // the connector's approved endpoint
	Body           []byte
	IdempotencyKey string
	// EchoExpect is the reference the answer must echo when the profile names an echo_path.
	EchoExpect string
	Credential Credential
	// Signer signs the body for hmac auth (the vault signs where the provider accepts HMAC).
	Signer func(ctx context.Context, body []byte) (string, error)
}

// Transport performs calls through an egress-policy client.
type Transport struct {
	Client   *http.Client
	MaxBytes int64 // response body bound
}

// ErrRedirect is a 3xx: not followed, treated as a failure.
var ErrRedirect = errors.New("the provider redirected the request")

// Submit sends the profile's submit request.
func (t *Transport) Submit(ctx context.Context, c Call) Result {
	spec := c.Spec.Normalize()
	target, err := join(c.BaseURL, spec.Submit.Path)
	if err != nil {
		return Result{Disposition: NotSent, ErrCode: "bad_endpoint"}
	}
	return t.do(ctx, spec, spec.Submit.Method, target, c, spec.Response, nil, spec.Submit.ContentType)
}

// Fetch looks up the original submission (FetchOriginalAttemptStatus).
func (t *Transport) Fetch(ctx context.Context, c Call, providerRef string) Result {
	spec := c.Spec.Normalize()
	if spec.Query == nil {
		return Result{Disposition: NotSent, ErrCode: "no_query_profile"}
	}
	p := strings.NewReplacer(
		"{provider_ref}", url.PathEscape(providerRef),
		"{idempotency_key}", url.PathEscape(c.IdempotencyKey),
	).Replace(spec.Query.Path)
	target, err := join(c.BaseURL, p)
	if err != nil {
		return Result{Disposition: NotSent, ErrCode: "bad_endpoint"}
	}
	c.Body = nil
	return t.do(ctx, spec, spec.Query.Method, target, c, spec.Query.Response, spec.Query.NotFoundCodes, "")
}

func join(base, path string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", errors.New("bad endpoint")
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

func (t *Transport) do(ctx context.Context, spec Spec, method, target string, c Call, resp Response, notFound []int, ctype string) Result {
	max := t.MaxBytes
	if max <= 0 {
		max = 256 << 10
	}
	var body io.Reader
	if c.Body != nil {
		body = bytes.NewReader(c.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return Result{Disposition: NotSent, ErrCode: "bad_request"}
	}
	// net/http silently re-sends a request that has a body and an Idempotency-Key header when the server hangs
	// up on a reused connection after the request was written. That is a blind retry inside the library, and
	// it is exactly what this package exists to prevent: whether the first copy reached the provider is
	// unknown, and only the caller's own reconcile-then-resend flow may decide to send again. With no GetBody
	// the request is not replayable, so the transport never does.
	req.GetBody = nil
	if ctype != "" && c.Body != nil {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("Accept", "application/json")
	if spec.IdempotencyHeader != "" && c.IdempotencyKey != "" {
		req.Header.Set(spec.IdempotencyHeader, c.IdempotencyKey)
	}
	if err := applyAuth(ctx, req, spec.Auth, c); err != nil {
		return Result{Disposition: NotSent, ErrCode: "auth_setup"}
	}

	res := Result{RequestHash: hashBytes(c.Body)}
	var wrote atomic.Bool
	trace := &httptrace.ClientTrace{
		// Any request byte written, even to a connection that then dies, means the provider MAY have it.
		WroteHeaders: func() { wrote.Store(true) },
		WroteRequest: func(httptrace.WroteRequestInfo) { wrote.Store(true) },
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

	start := time.Now()
	hr, err := t.Client.Do(req)
	res.Duration = time.Since(start)
	if err != nil {
		if wrote.Load() {
			res.Disposition, res.ErrCode = Unknown, "no_answer"
		} else {
			res.Disposition, res.ErrCode = NotSent, "not_sent"
		}
		return res
	}
	defer hr.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(hr.Body, max+1))
	res.Disposition = Answered
	res.HTTPStatus = hr.StatusCode
	res.ResponseHash = hashBytes(raw)
	res.RetryAfter = parseRetryAfter(hr.Header.Get("Retry-After"))
	if rerr != nil || int64(len(raw)) > max {
		res.Class, res.ErrCode = ClassUnparseable, "answer_unreadable"
		return res
	}
	classify(&res, raw, resp, notFound, spec.Response.SuccessCodes, c.EchoExpect)
	return res
}

func applyAuth(ctx context.Context, req *http.Request, a Auth, c Call) error {
	switch a.Type {
	case "none", "":
		return nil
	case "bearer":
		if len(c.Credential.Secret) == 0 {
			return errors.New("credential required")
		}
		req.Header.Set("Authorization", "Bearer "+string(c.Credential.Secret))
	case "api_key":
		if len(c.Credential.Secret) == 0 {
			return errors.New("credential required")
		}
		req.Header.Set(a.Header, a.Prefix+string(c.Credential.Secret))
	case "hmac":
		if c.Signer == nil {
			return errors.New("signer required")
		}
		sig, err := c.Signer(ctx, c.Body)
		if err != nil {
			return err
		}
		req.Header.Set(a.SignatureHeader, sig)
	}
	return nil
}

func classify(res *Result, raw []byte, resp Response, notFound []int, success []int, echo string) {
	code := res.HTTPStatus
	switch {
	case code == http.StatusTooManyRequests:
		res.Class = ClassRateLimited
		return
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		res.Class = ClassAuth
		return
	case code >= 300 && code < 400:
		res.Class, res.ErrCode = ClassUnavailable, "redirect"
		return
	case code >= 500:
		res.Class = ClassUnavailable
		return
	}
	for _, n := range notFound {
		if code == n {
			res.Class = ClassNotFound
			return
		}
	}
	isSuccess := false
	for _, s := range success {
		if code == s {
			isSuccess = true
		}
	}
	if !isSuccess {
		// a 4xx the profile does not call success: the provider refused this request
		res.Class = ClassRejected
		res.Reason = reasonFrom(raw, resp)
		return
	}
	var doc any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		res.Class, res.ErrCode = ClassUnparseable, "answer_not_json"
		return
	}
	if resp.RefPath != "" {
		if v, ok := getPath(doc, resp.RefPath); ok {
			res.ProviderRef = clip(asString(v), 128)
		}
	}
	if resp.EchoPath != "" {
		v, ok := getPath(doc, resp.EchoPath)
		if !ok || echo == "" || asString(v) != echo {
			res.Class, res.ErrCode = ClassMismatch, "receipt_mismatch"
			return
		}
	}
	res.Class = ClassAcknowledged
	if resp.StatusPath != "" {
		v, ok := getPath(doc, resp.StatusPath)
		st := asString(v)
		mapped, known := resp.StatusMap[st]
		if !ok || !known {
			// the provider said something the profile does not define: do not guess
			res.Class, res.ErrCode = ClassUnparseable, "unmapped_status"
			return
		}
		res.ProviderStat = clip(st, 64)
		switch mapped {
		case StatusRejected:
			res.Class = ClassRejected
			res.Reason = reasonFrom(raw, resp)
		case StatusAccepted:
			res.Class = ClassAccepted
		case StatusPending:
			res.Class = ClassPending
		}
	}
	if res.Class == ClassAcknowledged && resp.RefPath != "" && res.ProviderRef == "" {
		res.Class, res.ErrCode = ClassUnparseable, "no_provider_ref"
	}
}

func reasonFrom(raw []byte, resp Response) string {
	if resp.ReasonPath == "" {
		return ""
	}
	var doc any
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	if v, ok := getPath(doc, resp.ReasonPath); ok {
		return clip(asString(v), 200)
	}
	return ""
}

func parseRetryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if n, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && n >= 0 {
		if n > 3600 {
			n = 3600
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0
		}
		if d > time.Hour {
			d = time.Hour
		}
		return d
	}
	return 0
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func hashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hashJSON(v any) string {
	b, _ := json.Marshal(v)
	return hashBytes(b)
}

// HMACSigner returns a local signer (tests and callers that legitimately hold the key).
func HMACSigner(key []byte) func(context.Context, []byte) (string, error) {
	return func(_ context.Context, body []byte) (string, error) {
		m := hmac.New(sha256.New, key)
		m.Write(body)
		return hex.EncodeToString(m.Sum(nil)), nil
	}
}

// DecodeCredential decodes the base64 credential the vault returns.
func DecodeCredential(b64 string) (Credential, error) {
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(b) == 0 {
		return Credential{}, fmt.Errorf("bad credential encoding")
	}
	return Credential{Secret: b}, nil
}

// HashBytes is the evidence hash (SHA-256, hex) used for request and response bodies.
func HashBytes(b []byte) string { return hashBytes(b) }
