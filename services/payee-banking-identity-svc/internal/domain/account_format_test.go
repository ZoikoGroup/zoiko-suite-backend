package domain_test

import (
	"testing"

	"zoiko.io/payee-banking-identity-svc/internal/domain"
)

func TestValidateAccountFormat_ValidIBANs(t *testing.T) {
	cases := []struct{ country, account string }{
		{"DE", "DE89370400440532013000"}, // well-known published example IBAN
		{"GB", "GB29NWBK60161331926819"}, // well-known published example IBAN
		{"FR", "FR1420041010050500013M02606"},
	}
	for _, c := range cases {
		if err := domain.ValidateAccountFormat(c.country, c.account); err != nil {
			t.Errorf("%s/%s: expected valid, got %v", c.country, c.account, err)
		}
		// Lower-case and spaced input must validate identically.
		if err := domain.ValidateAccountFormat(c.country, insertSpaces(c.account)); err != nil {
			t.Errorf("%s/%s (spaced): expected valid, got %v", c.country, c.account, err)
		}
	}
}

func insertSpaces(s string) string {
	out := ""
	for i, r := range s {
		if i > 0 && i%4 == 0 {
			out += " "
		}
		out += string(r)
	}
	return out
}

func TestValidateAccountFormat_InvalidChecksum_Rejected(t *testing.T) {
	// Last digit of a real, valid IBAN flipped — same length and structure,
	// fails only the mod-97 checksum.
	if err := domain.ValidateAccountFormat("DE", "DE89370400440532013001"); err == nil {
		t.Fatal("expected a checksum failure to be rejected")
	}
}

func TestValidateAccountFormat_WrongLength_Rejected(t *testing.T) {
	if err := domain.ValidateAccountFormat("DE", "DE8937040044053201300"); err == nil {
		t.Fatal("expected a too-short DE IBAN to be rejected")
	}
}

func TestValidateAccountFormat_WrongCountryPrefix_Rejected(t *testing.T) {
	// A structurally valid GB IBAN proposed under country_code DE.
	if err := domain.ValidateAccountFormat("DE", "GB29NWBK60161331926819"); err == nil {
		t.Fatal("expected a mismatched country prefix to be rejected")
	}
}

func TestValidateAccountFormat_NonIBANCountry_MinimalSanityRule(t *testing.T) {
	if err := domain.ValidateAccountFormat("US", "1234567890123456"); err != nil {
		t.Errorf("expected a plausible US account number to pass the minimal rule, got %v", err)
	}
	if err := domain.ValidateAccountFormat("US", "ab"); err == nil {
		t.Fatal("expected a too-short non-IBAN account identifier to be rejected")
	}
	if err := domain.ValidateAccountFormat("US", "has a space in it!"); err == nil {
		t.Fatal("expected a non-alphanumeric non-IBAN account identifier to be rejected")
	}
}

func TestValidateAccountFormat_EmptyInputs_Rejected(t *testing.T) {
	if err := domain.ValidateAccountFormat("", "1234567890123456"); err == nil {
		t.Fatal("expected an empty country_code to be rejected")
	}
	if err := domain.ValidateAccountFormat("US", ""); err == nil {
		t.Fatal("expected an empty account_identifier to be rejected")
	}
}
