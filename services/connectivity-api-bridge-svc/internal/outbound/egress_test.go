package outbound_test

import (
	"net/http"
	"strings"
	"testing"

	"zoiko.io/connectivity-api-bridge-svc/internal/outbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
)

// ---- redirects follow the endpoint profile, never the other side (§9.4, §15.1, NP-29) --------------------

// unknownAttempt returns a connection and an UNKNOWN attempt (the submit timed out), ready for a status query whose
// answers the test scripts.
func (e *env) unknownOn(v variant) (*xicconn.Connection, *outbound.Attempt) {
	e.t.Helper()
	c := e.activeConn(tenantA, e.publish(v, nil), "acc-r", refAPIKey)
	return c, e.unknown(c, "PAY-1")
}

// scripted: the status query's first hop answers redirect, later hops answer per path; POSTs are never expected.
func (e *env) redirectChain(first string, then func(c call) (int, string, error)) {
	e.prov.set(func(c call) (int, string, error) {
		if c.method != http.MethodGet {
			return 200, `{}`, nil
		}
		if strings.HasSuffix(c.url, "/v1/payments/status?client_ref=PAY-1") || strings.Contains(c.url, "/v1/payments/status?client_ref=PAY-1") && !strings.Contains(c.url, "hop") {
			return 302, "->" + first, nil
		}
		return then(c)
	})
}

func TestEgress_SameHostRedirectIsFollowedWithTheCredentialAndRevalidated(t *testing.T) {
	e := newEnv(t)
	c, a := e.unknownOn(variant{redirects: "SAME_HOST"})
	e.advance(60 * 60 * 1e9)
	e.redirectChain("https://api.bank-x.com/v1/payments/status-hop?client_ref=PAY-1", func(c call) (int, string, error) {
		return 200, `{"status":"SETTLED","payment_id":"P-5"}`, nil
	})
	got, err := e.svc.Resolve(ctxFor(tenantA), other, a.AttemptID, outbound.ResolveInput{})
	if err != nil || got.State != outbound.StateSettled || got.ProviderReference != "P-5" {
		t.Fatalf("%v %+v", err, got)
	}
	g := e.prov.gets()
	if len(g) != 2 || g[0].header.Get("X-Api-Key") != apiKey || g[1].header.Get("X-Api-Key") != apiKey {
		t.Fatalf("a same-host redirect keeps the credential: %+v", g)
	}
	if !strings.HasPrefix(g[1].url, "https://api.bank-x.com/") {
		t.Fatalf("%s", g[1].url)
	}
	_ = c
}

func TestEgress_RedirectToAnotherHostIsBlockedWhereTheProfileIsSameHost_NP29(t *testing.T) {
	e := newEnv(t)
	_, a := e.unknownOn(variant{redirects: "SAME_HOST"})
	e.advance(60 * 60 * 1e9)
	e.redirectChain("https://evil.example/steal", func(call) (int, string, error) { return 200, `{"status":"SETTLED"}`, nil })
	got, err := e.svc.Resolve(ctxFor(tenantA), other, a.AttemptID, outbound.ResolveInput{})
	if err != nil || got.State != outbound.StateUnknown || got.AmbiguityReason != outbound.ReasonRedirectRefused {
		t.Fatalf("%v %+v", err, got)
	}
	if g := e.prov.gets(); len(g) != 1 {
		t.Fatalf("the redirected request was made: %d GETs", len(g))
	}
	if !hasType(e.repo.Types(), "integration.security.outbound-redirect-refused") {
		t.Fatalf("%v", e.repo.Types())
	}
}

func TestEgress_ApprovedHostRedirectStripsTheCredential(t *testing.T) {
	e := newEnv(t)
	_, a := e.unknownOn(variant{redirects: "APPROVED_HOSTS"})
	e.advance(60 * 60 * 1e9)
	e.redirectChain("https://files.bank-x.com/v1/payments/status-hop?client_ref=PAY-1", func(c call) (int, string, error) {
		return 200, `{"status":"SETTLED","payment_id":"P-6"}`, nil
	})
	if _, err := e.svc.Resolve(ctxFor(tenantA), other, a.AttemptID, outbound.ResolveInput{}); err != nil {
		t.Fatal(err)
	}
	g := e.prov.gets()
	if len(g) != 2 || g[0].header.Get("X-Api-Key") != apiKey {
		t.Fatalf("%+v", g)
	}
	if g[1].header.Get("X-Api-Key") != "" || g[1].header.Get("Authorization") != "" {
		t.Fatalf("a credential was forwarded across a host change: %v", g[1].header)
	}
	if !strings.HasPrefix(g[1].url, "https://files.bank-x.com/") {
		t.Fatalf("%s", g[1].url)
	}
}

func TestEgress_EveryHopIsRevalidatedAgainstEgressPolicy(t *testing.T) {
	// an approved host whose DNS now answers a private address (rebinding after approval)
	e := newEnv(t)
	_, a := e.unknownOn(variant{redirects: "APPROVED_HOSTS"})
	e.advance(60 * 60 * 1e9)
	e.redirectChain("https://rebind.bank-x.com/x", func(call) (int, string, error) { return 200, `{"status":"SETTLED"}`, nil })
	got, _ := e.svc.Resolve(ctxFor(tenantA), other, a.AttemptID, outbound.ResolveInput{})
	if got.State != outbound.StateUnknown || got.AmbiguityReason != outbound.ReasonRedirectRefused || len(e.prov.gets()) != 1 {
		t.Fatalf("%+v gets=%d", got, len(e.prov.gets()))
	}
}

func TestEgress_UnapprovedHostSchemeAndPortAreBlocked(t *testing.T) {
	for name, target := range map[string]string{
		"unapproved host":    "https://evil.example/x",
		"downgrade to http":  "http://api.bank-x.com/x",
		"another port":       "https://api.bank-x.com:8443/x",
		"credentials in url": "https://user:pw@api.bank-x.com/x",
	} {
		e := newEnv(t)
		_, a := e.unknownOn(variant{redirects: "APPROVED_HOSTS"})
		e.advance(60 * 60 * 1e9)
		e.redirectChain(target, func(call) (int, string, error) { return 200, `{"status":"SETTLED"}`, nil })
		got, _ := e.svc.Resolve(ctxFor(tenantA), other, a.AttemptID, outbound.ResolveInput{})
		if got.State != outbound.StateUnknown || len(e.prov.gets()) != 1 {
			t.Errorf("%s: %+v gets=%d", name, got, len(e.prov.gets()))
		}
	}
}

func TestEgress_RedirectChainsAreBounded(t *testing.T) {
	e := newEnv(t)
	_, a := e.unknownOn(variant{redirects: "SAME_HOST"})
	e.advance(60 * 60 * 1e9)
	hops := 0
	e.prov.set(func(c call) (int, string, error) {
		if c.method != http.MethodGet {
			return 200, `{}`, nil
		}
		hops++
		return 302, "->https://api.bank-x.com/loop", nil
	})
	got, _ := e.svc.Resolve(ctxFor(tenantA), other, a.AttemptID, outbound.ResolveInput{})
	if got.State != outbound.StateUnknown || hops > 5 {
		t.Fatalf("%+v hops=%d", got, hops)
	}
}

func TestEgress_NoneIsTheDefaultAndAGetRedirectIsNotFollowed(t *testing.T) {
	e := newEnv(t)
	_, a := e.unknownOn(variant{})
	e.advance(60 * 60 * 1e9)
	e.redirectChain("https://api.bank-x.com/elsewhere", func(call) (int, string, error) { return 200, `{"status":"SETTLED"}`, nil })
	got, _ := e.svc.Resolve(ctxFor(tenantA), other, a.AttemptID, outbound.ResolveInput{})
	if got.State != outbound.StateUnknown || len(e.prov.gets()) != 1 {
		t.Fatalf("%+v gets=%d", got, len(e.prov.gets()))
	}
}

func TestEgress_ASubmissionIsNeverRedirectedWhateverTheProfile(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(variant{redirects: "APPROVED_HOSTS"}, nil), "acc-p", refAPIKey)
	a := e.reserve(c, "PAY-1")
	e.prov.set(func(call) (int, string, error) { return 307, "->https://files.bank-x.com/v1/payments", nil })
	got, err := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil)
	if err != nil || got.State != outbound.StateFailed || got.AmbiguityReason != outbound.ReasonRedirectRefused {
		t.Fatalf("%v %+v", err, got)
	}
	if len(e.prov.posts()) != 1 {
		t.Fatal("a POST was replayed at another address")
	}
}

func TestEgress_TheDestinationIsEvidencedByHashNotByName(t *testing.T) {
	e := newEnv(t)
	c := e.setup(variant{})
	a := e.reserve(c, "PAY-1")
	if _, err := e.svc.Submit(ctxFor(tenantA), domain, a.AttemptID, nil); err != nil {
		t.Fatal(err)
	}
	evs, _ := e.repo.Events(ctxFor(tenantA), a.AttemptID)
	var started string
	for _, ev := range evs {
		if ev.Kind == "SUBMIT_STARTED" {
			started = string(ev.Detail)
		}
	}
	if !strings.Contains(started, "destination_hash") || strings.Contains(started, "bank-x") {
		t.Fatalf("%s", started)
	}
}
