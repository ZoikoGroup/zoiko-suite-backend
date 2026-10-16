package egress

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

type fakeResolver map[string][]string

func (f fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	ips, ok := f[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	var out []net.IPAddr
	for _, s := range ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	return out, nil
}

func TestIsPublicIP(t *testing.T) {
	for _, ip := range []string{
		"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111", "151.101.1.1",
	} {
		if !IsPublicIP(net.ParseIP(ip)) {
			t.Errorf("%s must be public", ip)
		}
	}
	for _, ip := range []string{
		"127.0.0.1", "127.255.255.254", "10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1",
		"169.254.169.254", "169.254.0.1", "100.64.0.1", "0.0.0.0", "0.1.2.3", "224.0.0.1", "255.255.255.255",
		"240.0.0.1", "198.18.0.1", "192.0.2.1", "198.51.100.7", "203.0.113.9",
		"::1", "::", "fe80::1", "fc00::1", "fd12:3456::1", "ff02::1", "2001:db8::1", "fd00:ec2::254", "64:ff9b::7f00:1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254", // IPv4-mapped
	} {
		if IsPublicIP(net.ParseIP(ip)) {
			t.Errorf("%s must NOT be public", ip)
		}
	}
	if IsPublicIP(nil) {
		t.Error("nil is not public")
	}
}

func TestValidateSyntax_INT_NP_018(t *testing.T) {
	p := Policy{}
	for name, u := range map[string]string{
		"http":               "http://api.bank-x.example/v1",
		"ftp":                "ftp://api.bank-x.example/",
		"userinfo":           "https://u:p@api.bank-x.example/",
		"loopback literal":   "https://127.0.0.1/x",
		"private literal":    "https://10.0.0.5/x",
		"metadata literal":   "https://169.254.169.254/latest/meta-data",
		"ipv6 loopback":      "https://[::1]/x",
		"ipv6 metadata":      "https://[fd00:ec2::254]/x",
		"mapped v4 loopback": "https://[::ffff:127.0.0.1]/x",
		"decimal ip":         "https://2130706433/x",
		"hex ip":             "https://0x7f000001/x",
		"octal ip":           "https://0177.0.0.1/x",
		"short ip":           "https://127.1/x",
		"localhost":          "https://localhost/x",
		"localhost dot":      "https://localhost./x",
		"single label":       "https://intranet/x",
		"metadata name":      "https://metadata.google.internal/x",
		".internal":          "https://erp.example.internal/api",
		".local":             "https://printer.local/x",
		".cluster.local":     "https://svc.ns.svc.cluster.local/x",
		"odd port":           "https://api.bank-x.example:22/x",
		"port zero":          "https://api.bank-x.example:0/x",
		"port out of range":  "https://api.bank-x.example:70000/x",
		"empty":              "",
		"no host":            "https:///x",
		"relative":           "/x",
	} {
		if _, err := p.ValidateSyntax(u); !errors.Is(err, ErrDenied) {
			t.Errorf("%s: %q was not denied (err=%v)", name, u, err)
		}
	}
	for _, u := range []string{
		"https://api.bank-x.example/v1/feed", "https://API.Bank-X.example:443/v1", "https://203.0.114.5/x",
		"https://api.bank-x.example./v1",
	} {
		if h, err := p.ValidateSyntax(u); err != nil || strings.ToUpper(h) != strings.ToUpper(strings.TrimSuffix(h, ".")) {
			t.Errorf("%q refused or host not normalised: %q %v", u, h, err)
		}
	}
}

func TestPorts(t *testing.T) {
	if _, err := (Policy{AllowedPorts: []int{443, 8443}}).ValidateSyntax("https://api.bank-x.example:8443/x"); err != nil {
		t.Errorf("an allowed port was refused: %v", err)
	}
	if _, err := (Policy{}).ValidateSyntax("https://api.bank-x.example:8443/x"); err == nil {
		t.Error("by default only 443 is allowed")
	}
	dev := Policy{AllowHTTP: true}
	if _, err := dev.ValidateSyntax("http://api.bank-x.example/x"); err != nil {
		t.Errorf("http must be allowed when explicitly enabled: %v", err)
	}
	if _, err := dev.ValidateSyntax("http://api.bank-x.example:8080/x"); err == nil {
		t.Error("an unlisted port stays refused even for http")
	}
}

func TestErrorsNeverNameTheDestination(t *testing.T) {
	_, err := (Policy{}).ValidateSyntax("https://10.9.8.7/secret-path")
	if err == nil || strings.Contains(err.Error(), "10.9.8.7") || strings.Contains(err.Error(), "secret-path") {
		t.Fatalf("error leaks the destination: %v", err)
	}
}

func TestCheckDestinationResolves_INT_NP_018(t *testing.T) {
	p := Policy{Resolver: fakeResolver{
		"api.bank-x.example":    {"93.184.216.34"},
		"both.bank-x.example":   {"93.184.216.34", "93.184.216.35"},
		"rebind.bank-x.example": {"93.184.216.34", "10.0.0.8"}, // one private answer among public ones
		"meta.bank-x.example":   {"169.254.169.254"},
		"v6.bank-x.example":     {"2606:4700:4700::1111"},
		"v6bad.bank-x.example":  {"fd00::1"},
		"empty.bank-x.example":  {},
	}}
	ctx := context.Background()
	for _, ok := range []string{"api.bank-x.example", "both.bank-x.example", "v6.bank-x.example"} {
		if _, err := p.CheckDestination(ctx, "https://"+ok+"/x"); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for name, host := range map[string]string{
		"rebinding to a private address": "rebind.bank-x.example",
		"resolves to metadata":           "meta.bank-x.example",
		"resolves to unique-local ipv6":  "v6bad.bank-x.example",
		"does not resolve":               "nxdomain.bank-x.example",
		"resolves to nothing":            "empty.bank-x.example",
	} {
		if _, err := p.CheckDestination(ctx, "https://"+host+"/x"); !errors.Is(err, ErrDenied) {
			t.Errorf("%s: %s was not denied (%v)", name, host, err)
		}
	}
	// a public literal needs no lookup
	if _, err := p.CheckDestination(ctx, "https://93.184.216.34/x"); err != nil {
		t.Errorf("public literal: %v", err)
	}
	// syntax failures never reach the resolver
	panicRes := Policy{Resolver: panicResolver{}}
	if _, err := panicRes.CheckDestination(ctx, "https://localhost/x"); !errors.Is(err, ErrDenied) {
		t.Errorf("localhost: %v", err)
	}
}

type panicResolver struct{}

func (panicResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	panic("the resolver must not be consulted for a destination that is refused on syntax")
}

func TestHostMatchesSuffix(t *testing.T) {
	suffixes := []string{"bank-x.example", ".partner.example"}
	for host, want := range map[string]bool{
		"bank-x.example":         true,
		"api.bank-x.example":     true,
		"a.b.bank-x.example":     true,
		"API.BANK-X.EXAMPLE":     true,
		"api.partner.example":    true,
		"partner.example":        true,
		"evilbank-x.example":     false,
		"bank-x.example.evil.io": false,
		"bank-x.exampl":          false,
		"":                       false,
	} {
		if got := HostMatchesSuffix(host, suffixes); got != want {
			t.Errorf("%q = %v, want %v", host, got, want)
		}
	}
	if HostMatchesSuffix("api.bank-x.example", nil) || HostMatchesSuffix("x", []string{"", " "}) {
		t.Error("an empty allowlist matches nothing: fail closed")
	}
}
