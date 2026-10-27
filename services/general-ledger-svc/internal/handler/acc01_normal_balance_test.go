package handler_test

// SPEC_DEVIATIONS A2/B11: normal_balance is derived from account_type and
// served, read-only, on every account response.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"zoiko.io/general-ledger-svc/internal/domain"
)

var wantNormalBalance = map[domain.AccountType]string{
	domain.AccountTypeAsset:     "DEBIT",
	domain.AccountTypeExpense:   "DEBIT",
	domain.AccountTypeLiability: "CREDIT",
	domain.AccountTypeEquity:    "CREDIT",
	domain.AccountTypeRevenue:   "CREDIT",
}

func TestAccountType_NormalBalance(t *testing.T) {
	for typ, want := range wantNormalBalance {
		if !domain.IsValidAccountType(typ) {
			t.Fatalf("%s is not a valid account type", typ)
		}
		if got := typ.NormalBalance(); got != want {
			t.Errorf("%s: got %q, want %q", typ, got, want)
		}
	}
	// Every valid type has a mapping: a sixth type added later without one
	// fails here rather than serving an empty normal_balance.
	for _, typ := range []domain.AccountType{"ASSET", "LIABILITY", "EQUITY", "REVENUE", "EXPENSE"} {
		if _, ok := wantNormalBalance[typ]; !ok || typ.NormalBalance() == "" {
			t.Errorf("%s has no normal balance", typ)
		}
	}
	// An unknown type is never guessed.
	if got := domain.AccountType("NOT_A_TYPE").NormalBalance(); got != "" {
		t.Errorf("unknown type: got %q, want empty", got)
	}
}

// normalBalanceOf decodes one account-shaped JSON object's account_type and
// normal_balance.
type normalBalanceOf struct {
	AccountCode   string             `json:"account_code"`
	AccountType   domain.AccountType `json:"account_type"`
	NormalBalance string             `json:"normal_balance"`
}

func TestAccountResponses_CarryNormalBalance(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	codes := map[domain.AccountType]string{}
	for typ := range wantNormalBalance {
		code := "acct-" + string(typ)
		codes[typ] = code
		rec := doRequest(r, http.MethodPost, "/v1/chart-of-accounts/", domain.CreateAccountRequest{
			AccountCode: code, AccountName: string(typ), AccountType: typ,
		}, "principal-1")
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", typ, rec.Code, rec.Body.String())
		}
		var created normalBalanceOf
		_ = json.Unmarshal(rec.Body.Bytes(), &created)
		if created.NormalBalance != wantNormalBalance[typ] {
			t.Errorf("CreateAccount %s: normal_balance %q", typ, created.NormalBalance)
		}
	}

	// GetAccount
	for typ, code := range codes {
		rec := doRequest(r, http.MethodGet, "/v1/chart-of-accounts/"+code, nil, "principal-1")
		if rec.Code != http.StatusOK {
			t.Fatalf("get %s: %d %s", code, rec.Code, rec.Body.String())
		}
		var got normalBalanceOf
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.NormalBalance != wantNormalBalance[typ] {
			t.Errorf("GetAccount %s: normal_balance %q, want %q", typ, got.NormalBalance, wantNormalBalance[typ])
		}
	}

	// ListAccounts
	rec := doRequest(r, http.MethodGet, "/v1/chart-of-accounts/", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var list []normalBalanceOf
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != len(wantNormalBalance) {
		t.Fatalf("expected %d accounts, got %d", len(wantNormalBalance), len(list))
	}
	for _, a := range list {
		if a.NormalBalance != wantNormalBalance[a.AccountType] {
			t.Errorf("ListAccounts %s: normal_balance %q, want %q", a.AccountCode, a.NormalBalance, wantNormalBalance[a.AccountType])
		}
	}

	// GetAccountAsOf (an AccountVersion, the other account shape)
	asOf := url.QueryEscape(time.Now().UTC().Add(time.Minute).Format(time.RFC3339))
	rec = doRequest(r, http.MethodGet, "/v1/chart-of-accounts/"+codes[domain.AccountTypeRevenue]+"/as-of?as_of="+asOf, nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("as-of: %d %s", rec.Code, rec.Body.String())
	}
	var version normalBalanceOf
	_ = json.Unmarshal(rec.Body.Bytes(), &version)
	if version.NormalBalance != "CREDIT" {
		t.Errorf("GetAccountAsOf REVENUE: normal_balance %q", version.NormalBalance)
	}
}

// normal_balance is output only: a request carrying one is not a way to set
// it, and the served value still follows the type.
func TestAccountResponses_NormalBalanceIsNotWritable(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/chart-of-accounts/", map[string]any{
		"account_code": "4000-Sales", "account_name": "Sales", "account_type": "REVENUE", "normal_balance": "DEBIT",
	}, "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var got normalBalanceOf
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.NormalBalance != "CREDIT" {
		t.Fatalf("a REVENUE account must be CREDIT-normal whatever the request said, got %q", got.NormalBalance)
	}
}
