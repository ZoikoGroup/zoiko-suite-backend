package model

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// ErrForbiddenEgress: the model client was pointed at a cloud metadata,
// link-local, unspecified or multicast address (Doc 06 SEC-T050, OPR-015).
// The check runs on the address actually dialled, after DNS, so a hostname
// that resolves to 169.254.169.254 is refused as well.
var ErrForbiddenEgress = errors.New("model egress to a metadata or link-local address is refused")

// forbiddenEgress reports whether ip is never a model provider: the cloud
// instance-metadata endpoints (169.254.169.254, fd00:ec2::254 and the rest
// of the link-local ranges), the unspecified address and multicast.
func forbiddenEgress(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip == netip.MustParseAddr("fd00:ec2::254") || ip == netip.MustParseAddr("100.100.100.200") // AWS IPv6, Alibaba metadata
}

// guardDial refuses a connection to a forbidden address before it is made.
func guardDial(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || forbiddenEgress(ip) {
		return ErrForbiddenEgress
	}
	return nil
}

// guardedTransport returns t (or the default transport) with the egress
// guard on every dial. A transport that is not an *http.Transport is a test
// double and is returned as is.
func guardedTransport(t http.RoundTripper) http.RoundTripper {
	base, ok := t.(*http.Transport)
	if t == nil {
		base, ok = http.DefaultTransport.(*http.Transport)
	}
	if !ok {
		return t
	}
	c := base.Clone()
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: guardDial}
	c.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return d.DialContext(ctx, network, addr)
	}
	return c
}
