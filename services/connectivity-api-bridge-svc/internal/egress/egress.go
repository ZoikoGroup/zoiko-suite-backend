// Package egress decides whether a connector's destination may be used at all
// (ZS-SVC-M-001 §7, INT-CTRL-015, INT-NP-018): an approved connector must not
// become arbitrary outbound network access.
//
// It is called when a connector is submitted and again when it is approved or
// reactivated, because DNS answers change: an endpoint that resolved to a public
// address last week can resolve to a private one today (DNS rebinding). Any
// future code that actually dials a connector's endpoint must call CheckDestination
// first and must not follow redirects to a destination that has not passed it.
package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// ErrDenied is returned for every refusal. Its message names the rule, never the
// destination, so a probe cannot learn what an internal resolver answered.
var ErrDenied = errors.New("egress denied")

// Resolver is the DNS lookup. A seam so tests need no network and so a policy
// can pin its resolver.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// Policy is a deployment's egress rules.
type Policy struct {
	// AllowHTTP permits http:// destinations. For local development only.
	AllowHTTP bool
	// AllowedPorts are the ports a destination may use. Empty means 443 only.
	AllowedPorts []int
	// Resolver defaults to the system resolver.
	Resolver Resolver
}

// ambiguousIPForm matches hosts that some resolvers read as an IP address in a
// form net.ParseIP does not recognise (2130706433, 0x7f.1, 0177.0.0.1, 127.1).
var ambiguousIPForm = regexp.MustCompile(`^[0-9a-fx.]+$`)

// blockedSuffixes are names that identify internal infrastructure.
var blockedSuffixes = []string{".local", ".localhost", ".internal", ".intranet", ".lan", ".home", ".corp", ".private", ".cluster.local", ".svc"}

// blockedNames are exact internal names, including cloud metadata endpoints.
var blockedNames = map[string]bool{
	"localhost": true, "metadata": true, "metadata.google.internal": true, "instance-data": true,
}

// ValidateSyntax checks what can be checked without the network: scheme, port,
// host shape, and that the host is not an IP literal in a blocked range or an
// internal name. It returns the lower-cased host.
func (p Policy) ValidateSyntax(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%w: destination is not a valid URL", ErrDenied)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !p.AllowHTTP {
			return "", fmt.Errorf("%w: destination must use https", ErrDenied)
		}
	default:
		return "", fmt.Errorf("%w: destination must use https", ErrDenied)
	}
	if u.User != nil {
		return "", fmt.Errorf("%w: destination must not carry credentials", ErrDenied)
	}
	if !p.portAllowed(u) {
		return "", fmt.Errorf("%w: destination port is not allowed", ErrDenied)
	}

	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return "", fmt.Errorf("%w: destination has no host", ErrDenied)
	}
	if ip := net.ParseIP(host); ip != nil {
		if !IsPublicIP(ip) {
			return "", fmt.Errorf("%w: destination address is not publicly routable", ErrDenied)
		}
		return host, nil
	}
	if ambiguousIPForm.MatchString(host) {
		return "", fmt.Errorf("%w: destination host looks like an IP address in an unsupported form", ErrDenied)
	}
	if !strings.Contains(host, ".") {
		return "", fmt.Errorf("%w: destination host must be a fully-qualified name", ErrDenied)
	}
	if blockedNames[host] {
		return "", fmt.Errorf("%w: destination host is an internal name", ErrDenied)
	}
	for _, s := range blockedSuffixes {
		if strings.HasSuffix(host, s) {
			return "", fmt.Errorf("%w: destination host is an internal name", ErrDenied)
		}
	}
	return host, nil
}

func (p Policy) portAllowed(u *url.URL) bool {
	portStr := u.Port()
	port := 443
	if u.Scheme == "http" {
		port = 80
	}
	if portStr != "" {
		n, err := strconv.Atoi(portStr)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
		port = n
	}
	allowed := p.AllowedPorts
	if len(allowed) == 0 {
		allowed = []int{443}
		if p.AllowHTTP {
			allowed = append(allowed, 80)
		}
	}
	for _, a := range allowed {
		if a == port {
			return true
		}
	}
	return false
}

// CheckDestination is ValidateSyntax plus resolution: every address the name
// resolves to must be publicly routable. One private answer among public ones is
// enough to refuse, because the dialer may pick any of them.
func (p Policy) CheckDestination(ctx context.Context, raw string) (string, error) {
	host, err := p.ValidateSyntax(raw)
	if err != nil {
		return "", err
	}
	if net.ParseIP(host) != nil {
		return host, nil // a literal was already judged
	}
	r := p.Resolver
	if r == nil {
		r = net.DefaultResolver
	}
	addrs, err := r.LookupIPAddr(ctx, host)
	if err != nil || len(addrs) == 0 {
		return "", fmt.Errorf("%w: destination host does not resolve", ErrDenied)
	}
	for _, a := range addrs {
		if !IsPublicIP(a.IP) {
			return "", fmt.Errorf("%w: destination host resolves to a non-public address", ErrDenied)
		}
	}
	return host, nil
}

// blockedNets are ranges that are never a legitimate public destination.
var blockedNets = mustCIDRs(
	"0.0.0.0/8",         // "this" network
	"10.0.0.0/8",        // private
	"100.64.0.0/10",     // carrier-grade NAT
	"127.0.0.0/8",       // loopback
	"169.254.0.0/16",    // link-local, including cloud metadata 169.254.169.254
	"172.16.0.0/12",     // private
	"192.0.0.0/24",      // IETF protocol assignments
	"192.0.2.0/24",      // documentation
	"192.88.99.0/24",    // deprecated 6to4 relay
	"192.168.0.0/16",    // private
	"198.18.0.0/15",     // benchmarking
	"198.51.100.0/24",   // documentation
	"203.0.113.0/24",    // documentation
	"224.0.0.0/4",       // multicast
	"240.0.0.0/4",       // reserved, includes broadcast
	"::/128",            // unspecified
	"::1/128",           // loopback
	"64:ff9b::/96",      // NAT64 (can embed a private IPv4)
	"100::/64",          // discard-only
	"2001:db8::/32",     // documentation
	"fc00::/7",          // unique local
	"fe80::/10",         // link-local
	"fec0::/10",         // deprecated site-local
	"ff00::/8",          // multicast
	"fd00:ec2::254/128", // AWS IPv6 metadata
)

func mustCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("egress: bad CIDR " + c)
		}
		out = append(out, n)
	}
	return out
}

// IsPublicIP reports whether ip is a publicly routable unicast address. An
// IPv4-mapped IPv6 address is judged as the IPv4 address it carries.
func IsPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

// HostMatchesSuffix reports whether host equals one of the allowed suffixes or
// is a subdomain of it (api.bank-x.example matches "bank-x.example"), on a
// label boundary: "evilbank-x.example" does not.
func HostMatchesSuffix(host string, suffixes []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, s := range suffixes {
		s = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "."))
		if s == "" {
			continue
		}
		if host == s || strings.HasSuffix(host, "."+s) {
			return true
		}
	}
	return false
}
