package envelope

import (
	"strings"
	"testing"
)

func scan(t *testing.T, js string) []string {
	t.Helper()
	return ScanSecrets([]byte(js))
}

// EV-T09: an event payload that contains an access token is blocked.
func TestEnvelopeNewBlocksAccessToken_EV_T09(t *testing.T) {
	s := validSpec()
	s.Data = map[string]any{"journal_id": "j-1", "provider": map[string]any{"access_token": "abc123"}}
	_, err := New(s)
	if err == nil {
		t.Fatal("a payload holding an access token must be refused")
	}
	if !strings.Contains(err.Error(), "data.provider.access_token") {
		t.Fatalf("error must name the path: %v", err)
	}
	if strings.Contains(err.Error(), "abc123") {
		t.Fatalf("error must never echo the secret: %v", err)
	}
}

func TestScanSecrets_ValuesThatAreSecretWhereverTheySit(t *testing.T) {
	cases := map[string]string{
		"jwt":        `{"note":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r"}`,
		"pem":        `{"x":"-----BEGIN RSA PRIVATE KEY-----\nMIIE"}`,
		"bearer":     `{"header":"Bearer abcdefghijklmnop1234567890"}`,
		"aws key id": `{"x":"AKIAIOSFODNN7EXAMPLE"}`,
		"url pwd":    `{"dsn":"postgres://app:hunter2secret@db:5432/x"}`,
		"card":       `{"memo":"4111 1111 1111 1111"}`,
		"nested arr": `{"a":[{"b":"Bearer abcdefghijklmnop1234567890"}]}`,
	}
	for name, js := range cases {
		if len(scan(t, js)) == 0 {
			t.Errorf("%s: not detected", name)
		}
	}
}

func TestScanSecrets_CredentialKeys(t *testing.T) {
	for _, k := range []string{"password", "Client-Secret", "api_key", "refreshToken", "private_key", "cvv", "card_number"} {
		if got := scan(t, `{"`+k+`":"x"}`); len(got) != 1 {
			t.Errorf("key %q with a value must be flagged, got %v", k, got)
		}
	}
	// an empty credential field holds nothing
	if got := scan(t, `{"password":""}`); len(got) != 0 {
		t.Errorf("empty value flagged: %v", got)
	}
}

// Ordinary business payloads must pass: a scanner that cries wolf gets bypassed.
func TestScanSecrets_NoFalsePositivesOnBusinessData(t *testing.T) {
	ok := []string{
		`{"journal_id":"0198f6c2-0000-7000-8000-000000000001","journal_number":"JE-2026-008142","debit_total":"1250.00"}`,
		`{"secret_ref":"vault://kv/prod/x","token_type":"opaque","authorization":"ALLOW","passwords_rotated":3}`,
		`{"iban_last4":"1234","reference":"1234 5678 9012 3456"}`, // 16 digits but fails Luhn
		`{"url":"https://example.com/path","email_domain":"example.com"}`,
		`{"period_key":"FY2026-P03","state":"HARD_CLOSED","amount":"4111"}`,
		`{"tx_hash":"sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"}`,
	}
	for _, js := range ok {
		if got := scan(t, js); len(got) != 0 {
			t.Errorf("false positive %v on %s", got, js)
		}
	}
}

func TestScanSecrets_ReportsPathsSortedAndNeverValues(t *testing.T) {
	got := scan(t, `{"b":{"password":"p1"},"a":{"password":"p2"}}`)
	if len(got) != 2 || got[0] != "data.a.password" || got[1] != "data.b.password" {
		t.Fatalf("got %v", got)
	}
	for _, g := range got {
		if strings.Contains(g, "p1") || strings.Contains(g, "p2") {
			t.Fatal("a path must not contain the value")
		}
	}
}

func TestLuhn(t *testing.T) {
	if !luhn("4111 1111 1111 1111") || luhn("4111 1111 1111 1112") || luhn("123") {
		t.Fatal("luhn broken")
	}
}
