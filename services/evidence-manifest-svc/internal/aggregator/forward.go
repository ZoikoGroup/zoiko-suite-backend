package aggregator

import (
	"context"
	"net/http"
	"strings"
)

// Forwarding the caller's Authorization bearer to authorization-svc for
// access-decision reads (register A-01).
//
// authorization-svc's GET /v1/access-decisions/{id} requires a verified
// identity envelope. When a person's request here leads to an access-decision
// read in that same person's name, their envelope (Authorization bearer) is
// the stronger proof and is forwarded. When this service acts with no person
// behind it (relay/scheduled path), it uses its mTLS identity as a registered
// delegate (AUTHZ_ADMIN_DELEGATES).

type forwardedKey struct{}

type forwarded struct {
	bearer    string
	principal string
}

// WithForwardedBearer records that requests made with ctx may carry bearer
// when they act for principal.
func WithForwardedBearer(ctx context.Context, bearer, principal string) context.Context {
	if bearer == "" || principal == "" {
		return ctx
	}
	return context.WithValue(ctx, forwardedKey{}, forwarded{bearer: bearer, principal: principal})
}

// ForwardBearer is middleware that records the caller's Authorization bearer
// and X-Principal-Id on the request context for the authorization-svc clients.
// It decides nothing: authorization-svc verifies the envelope itself.
func ForwardBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, token, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
		if ok && strings.EqualFold(scheme, "Bearer") && strings.TrimSpace(token) != "" {
			r = r.WithContext(WithForwardedBearer(r.Context(), strings.TrimSpace(token),
				strings.TrimSpace(r.Header.Get("X-Principal-Id"))))
		}
		next.ServeHTTP(w, r)
	})
}

// forwardingTransport adds the recorded bearer to an outgoing request that
// acts for the same principal, and to no other.
type forwardingTransport struct{ base http.RoundTripper }

func (t forwardingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") == "" {
		if f, ok := req.Context().Value(forwardedKey{}).(forwarded); ok &&
			f.principal == strings.TrimSpace(req.Header.Get("X-Principal-Id")) {
			req = req.Clone(req.Context())
			req.Header.Set("Authorization", "Bearer "+f.bearer)
		}
	}
	return t.base.RoundTrip(req)
}

// withForwarding returns a copy of hc whose transport forwards the bearer.
func withForwarding(hc *http.Client) *http.Client {
	out := *hc
	base := hc.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	out.Transport = forwardingTransport{base: base}
	return &out
}