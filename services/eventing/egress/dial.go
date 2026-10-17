package egress

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// DialContext returns a dial function that enforces the policy at connect time.
//
// A CheckDestination made before sending proves what the name resolved to then; the
// HTTP client resolves again when it connects, and an attacker-controlled DNS answer
// can change in between (DNS rebinding). This dialer closes that gap: it resolves the
// host itself, refuses unless every address is publicly routable, and connects to
// one of those validated addresses directly, so the address that was judged is the
// address that is used.
func (p Policy) DialContext(d *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if d == nil {
		d = &net.Dialer{Timeout: 5 * time.Second}
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("%w: bad dial address", ErrDenied)
		}
		var ips []net.IP
		if ip := net.ParseIP(host); ip != nil {
			ips = []net.IP{ip}
		} else {
			r := p.Resolver
			if r == nil {
				r = net.DefaultResolver
			}
			addrs, err := r.LookupIPAddr(ctx, host)
			if err != nil || len(addrs) == 0 {
				return nil, fmt.Errorf("%w: destination host does not resolve", ErrDenied)
			}
			for _, a := range addrs {
				ips = append(ips, a.IP)
			}
		}
		for _, ip := range ips {
			if !p.Permitted(ip) {
				return nil, fmt.Errorf("%w: destination resolves to a non-public address", ErrDenied)
			}
		}
		var lastErr error
		for _, ip := range ips {
			conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
}

// NewHTTPClient builds the only HTTP client an outbound integration should use to
// call a tenant-configured destination: policy-enforcing dialer, TLS 1.2 minimum with
// full certificate verification, a hard overall timeout, bounded response headers, no
// proxy (a proxy would dial on the policy's behalf) and no redirect following (a 3xx
// to somewhere else has not passed the policy; it is returned to the caller as the
// response and treated as a failure).
func (p Policy) NewHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	tr := &http.Transport{
		Proxy:                  nil,
		DialContext:            p.DialContext(nil),
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  timeout,
		MaxResponseHeaderBytes: 16 << 10,
		MaxIdleConns:           64,
		MaxIdleConnsPerHost:    2,
		IdleConnTimeout:        30 * time.Second,
		DisableCompression:     true,
		ForceAttemptHTTP2:      true,
	}
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// HostOf returns the lower-cased host of a destination URL for evidence (never the
// path or query, which may carry identifiers).
func HostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
