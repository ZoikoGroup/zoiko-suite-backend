package govdecision

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func sample(now time.Time) *Decision {
	return &Decision{
		ContractVersion: ContractVersion, DecisionID: "dec-1", RequestID: "req-1", CorrelationID: "flow-1",
		Outcome: SignatureRequired, OwningService: "policy-svc", PolicyFamily: "SIGNATORY_MATRIX",
		RuleID: "pv-1#band:B2", PolicyVersion: "3", PolicyVersionID: "pv-1",
		TenantID: "t-1", LegalEntityID: "le-1", ActorPrincipalID: "person-200", Action: "SIGN_CONTRACT",
		Resource:    Resource{Type: "contract", ID: "C-1", Version: "4"},
		EvaluatedAt: now, ValidUntil: now.Add(5 * time.Minute), InputFingerprint: "sha256:abc",
		ReasonCodes:     []string{ReasonSignatureQuorumIncomplete},
		RequiredActions: []RequiredAction{{Kind: ActionKindSignatureQuorum, MinDistinctSigners: 2}},
		EvidenceRef:     "policy-svc:policy-evaluations/dec-1", UpstreamRefs: []string{},
	}
}

func expectation(now time.Time) Expectation {
	return Expectation{
		TenantID: "t-1", LegalEntityID: "le-1", ActorPrincipalID: "person-200", Action: "SIGN_CONTRACT",
		Resource: Resource{Type: "contract", ID: "C-1", Version: "4"}, OwningService: "policy-svc",
		PolicyFamily: "SIGNATORY_MATRIX", Now: now,
	}
}

func signed(t *testing.T, s *Signer, d *Decision) []byte {
	t.Helper()
	raw, err := s.Sign(d)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestOutcomes_UnknownIsErrorAndOnlyPermitExecutes(t *testing.T) {
	for _, o := range Outcomes {
		if Normalize(o) != o {
			t.Fatalf("%s normalised away", o)
		}
		if Executable(o) != (o == Permit) {
			t.Fatalf("%s executable=%v", o, Executable(o))
		}
	}
	for _, o := range []string{"", "ALLOW", "permit", "GRANTED", "WITHIN_THRESHOLD"} {
		if Normalize(o) != Error || Effect(o) != EffectBlock || Executable(o) {
			t.Fatalf("%q must normalise to ERROR and block", o)
		}
	}
	for o, want := range map[string]string{
		Permit: EffectContinue, ApprovalRequired: EffectHold, SignatureRequired: EffectHold, ReviewRequired: EffectHold,
		Deny: EffectBlock, NotConfigured: EffectBlock, NotImplemented: EffectBlock, Error: EffectBlock,
	} {
		if Effect(o) != want {
			t.Fatalf("Effect(%s) = %s, want %s", o, Effect(o), want)
		}
	}
}

func TestReasonCodes_UniqueAndUpperSnake(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range ReasonCodes {
		if seen[c] || strings.ToUpper(c) != c || strings.Contains(c, " ") {
			t.Fatalf("reason code %q duplicated or malformed", c)
		}
		seen[c] = true
	}
}

func TestSignAndCheck_TrustedDecision(t *testing.T) {
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	s, err := NewRandomSigner()
	if err != nil {
		t.Fatal(err)
	}
	raw := signed(t, s, sample(now))
	exp := expectation(now)
	exp.Keys = KeySet{s.KeyID: s.Public()}
	v := Check(raw, exp)
	if !v.Trusted() || v.Outcome != SignatureRequired {
		t.Fatalf("verdict %+v", v)
	}
	// A different JSON key order verifies the same: the signature covers the
	// canonical form.
	var generic map[string]any
	_ = json.Unmarshal(raw, &generic)
	reordered, _ := json.MarshalIndent(generic, "", "  ")
	if v := Check(reordered, exp); !v.Trusted() {
		t.Fatalf("re-encoded decision refused: %v", v.Problems)
	}
}

// C02-P0-01, -03, -04, -13, -14: every untrustworthy decision is ERROR.
func TestCheck_EveryProblemIsLocalError(t *testing.T) {
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	s, _ := NewRandomSigner()
	other, _ := NewRandomSigner()
	keys := KeySet{s.KeyID: s.Public()}

	cases := map[string]struct {
		mutate func(*Decision)
		exp    func(*Expectation)
		raw    func([]byte) []byte
		signer *Signer
	}{
		"unknown outcome":     {mutate: func(d *Decision) { d.Outcome = "ALLOW" }},
		"tenant mismatch":     {mutate: func(d *Decision) { d.TenantID = "t-2" }},
		"entity mismatch":     {mutate: func(d *Decision) { d.LegalEntityID = "le-2" }},
		"actor mismatch":      {mutate: func(d *Decision) { d.ActorPrincipalID = "person-201" }},
		"resource changed":    {exp: func(e *Expectation) { e.Resource.Version = "5" }},
		"wrong owner":         {mutate: func(d *Decision) { d.OwningService = "spend-controls-svc" }},
		"future dated":        {mutate: func(d *Decision) { d.EvaluatedAt = now.Add(time.Hour); d.ValidUntil = now.Add(2 * time.Hour) }},
		"expired":             {mutate: func(d *Decision) { d.ValidUntil = now }},
		"missing validity":    {mutate: func(d *Decision) { d.ValidUntil = time.Time{} }},
		"no reasons":          {mutate: func(d *Decision) { d.ReasonCodes = nil }},
		"missing evidence":    {mutate: func(d *Decision) { d.EvidenceRef = "" }},
		"major version":       {mutate: func(d *Decision) { d.ContractVersion = "2.0" }},
		"fingerprint":         {exp: func(e *Expectation) { e.InputFingerprint = "sha256:other" }},
		"unknown signing key": {signer: other},
		"tampered after sign": {raw: func(b []byte) []byte {
			return []byte(strings.Replace(string(b), `"SIGNATURE_REQUIRED"`, `"PERMIT"`, 1))
		}},
		"unsigned": {raw: func(b []byte) []byte {
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			delete(m, "integrity")
			out, _ := json.Marshal(m)
			return out
		}},
		"not json": {raw: func([]byte) []byte { return []byte("<html>") }},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d := sample(now)
			if c.mutate != nil {
				c.mutate(d)
			}
			signer := s
			if c.signer != nil {
				signer = c.signer
			}
			raw := signed(t, signer, d)
			if c.raw != nil {
				raw = c.raw(raw)
			}
			exp := expectation(now)
			exp.Keys = keys
			if c.exp != nil {
				c.exp(&exp)
			}
			v := Check(raw, exp)
			if v.Trusted() || v.Outcome != Error {
				t.Fatalf("verdict %+v, want local ERROR", v)
			}
		})
	}
}

func TestJWKS_RoundTrip(t *testing.T) {
	s, _ := NewSigner(make([]byte, 32), "")
	raw, _ := json.Marshal(JWKS{Keys: []JWK{s.JWK(), {KeyType: "RSA", KeyID: "x"}}})
	keys, err := ParseJWKS(raw)
	if err != nil || len(keys) != 1 || !keys[s.KeyID].Equal(s.Public()) {
		t.Fatalf("keys %v err %v", keys, err)
	}
	if _, err := ParseJWKS([]byte(`{"keys":[]}`)); err == nil {
		t.Fatal("an empty set must be refused")
	}
	if _, err := NewSigner([]byte("short"), ""); err == nil {
		t.Fatal("a short seed must be refused")
	}
}

func TestFingerprint_IndependentOfKeyOrder(t *testing.T) {
	a, _ := Fingerprint(map[string]any{"a": 1, "b": "x"})
	b, _ := Fingerprint(json.RawMessage(`{"b":"x","a":1}`))
	if a != b || !strings.HasPrefix(a, "sha256:") {
		t.Fatalf("%s != %s", a, b)
	}
}
