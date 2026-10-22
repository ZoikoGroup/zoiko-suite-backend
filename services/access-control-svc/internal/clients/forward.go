package clients

import (
	"context"
	"net/http"
	"strings"
)

// Forwarding the caller's identity envelope to authorization-svc (its
// register A-01).
//
// authorization-svc no longer takes X-Principal-Id on trust on its admin
// routes: a caller proves who it is with the person's identity envelope, or —
// for this service acting on its own schedule — with its mTLS certificate as a
// registered delegate (AUTHZ_ADMIN_DELEGATES). When a person's request here
// leads straight to an admin call in that same person's name, their envelope
// is the stronger proof and is forwarded. When this service acts for someone
// else (executing a request another person approved, an HR-driven revocation)
// it is not, because the envelope would name the wrong principal; those calls
// rely on the delegate certificate.

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
