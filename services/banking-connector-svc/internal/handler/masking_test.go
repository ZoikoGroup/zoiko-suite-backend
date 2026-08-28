package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/banking-connector-svc/internal/domain"
)

// TestMaskDigits pins the exact masking behavior: everything but the last
// four characters is replaced with "*", and short values are masked
// entirely rather than left partially or fully visible.
func TestMaskDigits(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"1234", "****"},
		{"12", "**"},
		{"GB89CHAS1234567890", "**************7890"},
	}
	for _, tc := range cases {
		if got := maskDigits(tc.in); got != tc.want {
			t.Fatalf("maskDigits(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestGetConnectionByID_MasksAccountNumberAndIBAN is the regression test
// for the master register's masking invariant (ZS-SVC-F-001-derived
// invariant #2): AccountNumber and IBAN must never appear in the clear in
// a read API, since the caller of GetConnectionByID may be a different
// principal in the same tenant than whoever created the connection.
func TestGetConnectionByID_MasksAccountNumberAndIBAN(t *testing.T) {
	r, _ := setupTestRouter(t)

	connID := createConnectionAs(t, r, "tenant-mask", "le-mask")

	req := httptest.NewRequest("GET", "/v1/banking/connections/"+connID, nil)
	req.Header.Set("X-Tenant-Id", "tenant-mask")
	req.Header.Set("X-Principal-Id", "principal-01")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if strings.Contains(w.Body.String(), "GB00TEST1234567890") {
		t.Fatalf("LEAK: raw account number/IBAN present in get-connection response: %s", w.Body.String())
	}

	var got domain.BankConnection
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.AccountNumber != "**************7890" {
		t.Fatalf("expected masked account_number, got %q", got.AccountNumber)
	}
	if got.IBAN != "**************7890" {
		t.Fatalf("expected masked iban, got %q", got.IBAN)
	}
}

// TestListConnections_MasksAccountNumberAndIBAN mirrors the get-by-id
// check for the list route, including both the "connections" and
// "accounts" response keys the route repeats the same rows under.
func TestListConnections_MasksAccountNumberAndIBAN(t *testing.T) {
	r, _ := setupTestRouter(t)

	createConnectionAs(t, r, "tenant-mask-list", "le-mask-list")

	req := httptest.NewRequest("GET", "/v1/banking/connections", nil)
	req.Header.Set("X-Tenant-Id", "tenant-mask-list")
	req.Header.Set("X-Principal-Id", "principal-01")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if strings.Contains(w.Body.String(), "GB00TEST1234567890") {
		t.Fatalf("LEAK: raw account number/IBAN present in list-connections response: %s", w.Body.String())
	}

	var got struct {
		Connections []domain.BankConnection `json:"connections"`
		Accounts    []domain.BankConnection `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for key, list := range map[string][]domain.BankConnection{"connections": got.Connections, "accounts": got.Accounts} {
		if len(list) != 1 {
			t.Fatalf("%s: expected 1 connection, got %d", key, len(list))
		}
		if list[0].AccountNumber != "**************7890" {
			t.Fatalf("%s: expected masked account_number, got %q", key, list[0].AccountNumber)
		}
		if list[0].IBAN != "**************7890" {
			t.Fatalf("%s: expected masked iban, got %q", key, list[0].IBAN)
		}
	}
}

// TestCreateConnection_ResponseNotMasked pins the deliberate asymmetry:
// CreateConnection's own response echoes back the value the caller just
// submitted, so it is not masked. Masking a value back to the same
// principal who just sent it would add no security benefit while breaking
// any client that expects to see what it sent.
func TestCreateConnection_ResponseNotMasked(t *testing.T) {
	r, _ := setupTestRouter(t)

	body, _ := json.Marshal(domain.CreateConnectionRequest{
		LegalEntityID: "le-create-unmasked",
		BankName:      "Test Bank",
		AccountNumber: "GB00TEST1234567890",
		Currency:      "USD",
	})
	req := httptest.NewRequest("POST", "/v1/banking/connections", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", "tenant-create-unmasked")
	req.Header.Set("X-Principal-Id", "principal-01")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var got domain.BankConnection
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.AccountNumber != "GB00TEST1234567890" {
		t.Fatalf("expected create response to echo the submitted account_number unmasked, got %q", got.AccountNumber)
	}
}
