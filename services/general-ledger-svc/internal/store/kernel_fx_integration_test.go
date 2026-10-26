package store_test

import (
	"testing"

	"github.com/google/uuid"
)

// Exchange rates (standard §23.1) against real Postgres: sourced and immutable, resolved by policy, never stale, never silently
// substituted, and overridden only by a second person.

func (e *kenv) rate(base, quote, rate, typ, at, src, id string) (int, map[string]any) {
	return e.call("POST", "/v1/fx-rates", e.tenant, "treasury-feed", map[string]any{"base_currency": base, "quote_currency": quote, "rate": rate, "rate_type": typ,
		"effective_at": at, "source": src, "source_rate_id": id})
}

func (e *kenv) resolve(from, to, date, policy string) (int, map[string]any) {
	return e.call("GET", "/v1/fx-rate-resolutions?from="+from+"&to="+to+"&date="+date+"&policy_id="+policy, e.tenant, "reader", nil)
}

func TestExchangeRatesAreSourcedImmutableAndResolvedByPolicy(t *testing.T) {
	e := newKEnv(t)
	st, r1 := e.rate("USD", "GBP", "0.80", "SPOT", "2026-03-10T08:00:00Z", "ecb", "r-1")
	if st != 201 || r1["rate"] != "0.8" || r1["rate_date"] != "2026-03-10" || r1["ingested_by"] != "treasury-feed" {
		t.Fatalf("%d %v", st, r1)
	}
	if st, again := e.rate("USD", "GBP", "0.80", "SPOT", "2026-03-10T08:00:00Z", "ecb", "r-1"); st != 200 || again["fx_rate_id"] != r1["fx_rate_id"] {
		t.Fatalf("the same source reference again is a repeat: %d %v", st, again)
	}
	if st, b := e.rate("USD", "GBP", "0.81", "SPOT", "2026-03-10T08:00:00Z", "ecb", "r-1"); st != 422 || b["code"] != "SOURCE_RATE_CONFLICT" {
		t.Fatalf("a different rate under the same reference is refused: %d %v", st, b)
	}
	for what, body := range map[string][]string{
		"same currency":   {"USD", "USD", "1", "SPOT"},
		"zero":            {"USD", "GBP", "0", "SPOT"},
		"13 decimals":     {"USD", "GBP", "0.1234567890123", "SPOT"},
		"unknown type":    {"USD", "GBP", "0.8", "WHATEVER"},
	} {
		if st, b := e.rate(body[0], body[1], body[2], body[3], "2026-03-10T08:00:00Z", "ecb", "bad-"+what); st != 422 && st != 400 {
			t.Fatalf("%s: %d %v", what, st, b)
		}
	}
	wantRefused(t, "change a rate", e.asApp(e.tenant, `UPDATE fx_rates SET rate = 1`), "append-only")
	wantRefused(t, "delete a rate", e.asApp(e.tenant, `DELETE FROM fx_rates`), "append-only")

	// resolution: the policy picks the type, the latest on or before the date, and the age limit decides what is too old
	st, b := e.resolve("USD", "GBP", "2026-03-10", "SPOT_ON_DATE")
	if st != 200 || b["rate"] != "0.8" || b["rate_type"] != "SPOT" || b["rate_id"] != r1["fx_rate_id"] || b["as_of"] != "2026-03-10" {
		t.Fatalf("%d %v", st, b)
	}
	e.rate("USD", "GBP", "0.82", "SPOT", "2026-03-11T08:00:00Z", "ecb", "r-2")
	if _, b = e.resolve("USD", "GBP", "2026-03-10", ""); b["rate"] != "0.8" {
		t.Fatalf("a later rate is not used for an earlier date: %v", b)
	}
	if _, b = e.resolve("USD", "GBP", "2026-03-12", ""); b["rate"] != "0.82" {
		t.Fatalf("the latest on or before the date: %v", b)
	}
	// three days old is allowed by the spot policy, four is not
	if st, b = e.resolve("USD", "GBP", "2026-03-14", "SPOT_ON_DATE"); st != 200 || b["rate"] != "0.82" {
		t.Fatalf("%d %v", st, b)
	}
	if st, b = e.resolve("USD", "GBP", "2026-03-15", "SPOT_ON_DATE"); st != 422 || b["code"] != "RATE_STALE" || b["family"] != "AK-FX" {
		t.Fatalf("a stale rate is refused, not used: %d %v", st, b)
	}
	// the closing policy needs a closing rate on the day: a spot rate does not stand in for it
	if st, b = e.resolve("USD", "GBP", "2026-03-31", "CLOSING_ON_DATE"); st != 422 || b["code"] != "RATE_MISSING" {
		t.Fatalf("an alternate rate type is never substituted: %d %v", st, b)
	}
	e.rate("USD", "GBP", "0.79", "CLOSING", "2026-03-31T17:00:00Z", "ecb", "c-1")
	if st, b = e.resolve("USD", "GBP", "2026-03-31", "CLOSING_ON_DATE"); st != 200 || b["rate"] != "0.79" || b["rate_type"] != "CLOSING" {
		t.Fatalf("%d %v", st, b)
	}
	if st, b = e.resolve("USD", "GBP", "2026-04-01", "CLOSING_ON_DATE"); st != 422 || b["code"] != "RATE_STALE" {
		t.Fatalf("the closing rate is for its own day: %d %v", st, b)
	}
	// the other direction is derived from the stored pair, and says so
	if st, b = e.resolve("GBP", "USD", "2026-03-10", "SPOT_ON_DATE"); st != 200 || b["rate"] != "1.25" || b["rate_id"] != r1["fx_rate_id"].(string)+":inverse" {
		t.Fatalf("%d %v", st, b)
	}
	if st, b = e.resolve("USD", "JPY", "2026-03-10", "SPOT_ON_DATE"); st != 422 || b["code"] != "RATE_MISSING" {
		t.Fatalf("no triangulation: %d %v", st, b)
	}
	if st, _ = e.resolve("USD", "GBP", "2026-03-10", "NO_SUCH_POLICY"); st != 422 {
		t.Fatalf("an unknown policy: %d", st)
	}
	st, list := e.call("GET", "/v1/fx-rates?base_currency=USD&quote_currency=GBP&rate_type=SPOT", e.tenant, "reader", nil)
	if st != 200 || list["total"].(float64) != 2 {
		t.Fatalf("%d %v", st, list)
	}
	// another tenant sees none of it
	if st, b = e.call("GET", "/v1/fx-rate-resolutions?from=USD&to=GBP&date=2026-03-10", uuid.NewString(), "reader", nil); st != 422 || b["code"] != "RATE_MISSING" {
		t.Fatalf("%d %v", st, b)
	}
}

func TestAHandKeyedRateNeedsASecondPersonsApproval(t *testing.T) {
	e := newKEnv(t)
	req := map[string]any{"base_currency": "EUR", "quote_currency": "GBP", "rate": "0.86", "rate_type": "SPOT", "effective_at": "2026-03-10T08:00:00Z"}
	if st, b := e.call("POST", "/v1/fx-rate-overrides", e.tenant, "dealer", req); st != 422 || b["code"] != "OVERRIDE_REASON_REQUIRED" {
		t.Fatalf("a justification is required: %d %v", st, b)
	}
	req["reason"] = "provider outage, desk quote 09:14"
	st, o := e.call("POST", "/v1/fx-rate-overrides", e.tenant, "dealer", req)
	if st != 201 || o["status"] != "PENDING" {
		t.Fatalf("%d %v", st, o)
	}
	id := o["request_id"].(string)
	// it is not a rate yet
	if st, b := e.resolve("EUR", "GBP", "2026-03-10", "SPOT_ON_DATE"); st != 422 || b["code"] != "RATE_MISSING" {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := e.call("POST", "/v1/fx-rate-overrides/"+id+"/approve", e.tenant, "dealer", map[string]any{}); st != 403 || b["error"] != "separation_of_duties" {
		t.Fatalf("the requester cannot approve: %d %v", st, b)
	}
	wantRefused(t, "SQL self-approval", e.asApp(e.tenant, `UPDATE fx_rate_override_requests SET status='APPROVED', decided_by=requested_by, fx_rate_id=gen_random_uuid() WHERE request_id=$1::uuid`, id), "")
	wantRefused(t, "SQL override rate without an approver", e.asApp(e.tenant, `INSERT INTO fx_rates (fx_rate_id, tenant_id, base_currency, quote_currency, rate, rate_type, effective_at, rate_date, source, source_rate_id, ingested_by, override, override_reason)
		VALUES (gen_random_uuid(), $1::uuid, 'EUR', 'GBP', 0.9, 'SPOT', now(), current_date, 'x', 'y', 'dealer', true, 'because')`, e.tenant), "")
	st, dec := e.call("POST", "/v1/fx-rate-overrides/"+id+"/approve", e.tenant, "treasurer", map[string]any{})
	if st != 200 || dec["override"].(map[string]any)["status"] != "APPROVED" {
		t.Fatalf("%d %v", st, dec)
	}
	rate := dec["rate"].(map[string]any)
	if rate["override"] != true || rate["override_approved_by"] != "treasurer" || rate["override_reason"] == "" {
		t.Fatalf("%v", rate)
	}
	if st, b := e.resolve("EUR", "GBP", "2026-03-10", "SPOT_ON_DATE"); st != 200 || b["rate"] != "0.86" || b["source"] != "OVERRIDE" || b["rate_id"] != rate["fx_rate_id"] {
		t.Fatalf("%d %v", st, b)
	}
	if st, _ := e.call("POST", "/v1/fx-rate-overrides/"+id+"/approve", e.tenant, "treasurer2", map[string]any{}); st != 409 {
		t.Fatalf("a decided request is final: %d", st)
	}
	// rejection needs a reason and creates no rate
	req["rate"], req["effective_at"] = "0.50", "2026-03-11T08:00:00Z"
	_, o2 := e.call("POST", "/v1/fx-rate-overrides", e.tenant, "dealer", req)
	id2 := o2["request_id"].(string)
	if st, _ := e.call("POST", "/v1/fx-rate-overrides/"+id2+"/reject", e.tenant, "treasurer", map[string]any{}); st != 422 {
		t.Fatalf("a rejection needs its reason: %d", st)
	}
	if st, b := e.call("POST", "/v1/fx-rate-overrides/"+id2+"/reject", e.tenant, "treasurer", map[string]any{"note": "off market"}); st != 200 || b["override"].(map[string]any)["status"] != "REJECTED" {
		t.Fatalf("%d %v", st, b)
	}
	if e.count(t, `SELECT count(*) FROM fx_rates WHERE override`) != 1 {
		t.Fatal("only the approved override became a rate")
	}
	wantRefused(t, "reopen a decided request", e.asApp(e.tenant, `UPDATE fx_rate_override_requests SET status='PENDING' WHERE request_id=$1::uuid`, id), "final")
	st, list := e.call("GET", "/v1/fx-rate-overrides?status=PENDING", e.tenant, "reader", nil)
	if st != 200 || list["total"].(float64) != 0 {
		t.Fatalf("%d %v", st, list)
	}
}
