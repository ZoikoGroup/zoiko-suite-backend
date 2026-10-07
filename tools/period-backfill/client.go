package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// actor is the identity a request is made as. Only IDs are ever held or sent;
// the CLI has no credential of its own (the platform trusts the headers set by
// its gateway, exactly as the other service-to-service callers do).
type actor struct {
	principal string
	entity    string
}

type apiClient struct {
	http   *http.Client
	runID  string
	tenant string
}

func newClient(timeout time.Duration, tenant, runID string) *apiClient {
	return &apiClient{http: &http.Client{Timeout: timeout}, tenant: tenant, runID: runID}
}

// apiError is a non-2xx answer. Code/Message come from the service's JSON error
// body when it has one.
type apiError struct {
	Status  int
	Code    string
	Message string
	Method  string
	URL     string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s %s -> %d %s %s", e.Method, e.URL, e.Status, e.Code, e.Message)
}

// do performs one request. A nil out discards the body. idemKey != "" adds the
// Idempotency-Key header (all commands carry one; reads never do).
func (c *apiClient) do(ctx context.Context, a actor, method, url string, body any, idemKey string, out any) (int, http.Header, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Tenant-Id", c.tenant)
	req.Header.Set("X-Principal-Id", a.principal)
	if a.entity != "" {
		req.Header.Set("X-Legal-Entity-Id", a.entity)
	}
	req.Header.Set("X-Correlation-ID", c.runID)
	req.Header.Set("X-Source-System", "period-backfill")
	// The platform's write envelope (ZS-ARCH-SVC-001 §4) requires a request id and a
	// source channel on every command; "import" is the permitted channel for bulk
	// loads. Found by running this tool against the real services.
	req.Header.Set("X-Request-Id", newRequestID())
	req.Header.Set("X-Source-Channel", "import")
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		ae := &apiError{Status: resp.StatusCode, Method: method, URL: url}
		var eb struct {
			Code    string `json:"code"`
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &eb) == nil {
			ae.Code, ae.Message = eb.Code, eb.Message
			if ae.Code == "" {
				ae.Code = eb.Error
			}
		}
		if ae.Message == "" {
			ae.Message = truncate(string(raw), 200)
		}
		return resp.StatusCode, resp.Header, ae
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, resp.Header, fmt.Errorf("%s %s: undecodable response: %w", method, url, err)
		}
	}
	return resp.StatusCode, resp.Header, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// idemKey derives a deterministic Idempotency-Key from the logical operation,
// so a re-run (or a retry after a crash) replays instead of repeating.
func idemKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return "pbf-" + hex.EncodeToString(sum[:])[:40]
}

func isStatus(err error, code int) bool {
	ae, ok := err.(*apiError)
	return ok && ae.Status == code
}

// newRequestID returns a random RFC 4122 version-4 UUID. Every request gets its
// own, unlike X-Correlation-ID, which is shared by the whole run.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failing is not recoverable
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
