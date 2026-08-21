package domain_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"zoiko.io/accounts-payable-svc/internal/domain"
)

// due_date is a DATE column: it names a day. Accepting only RFC3339 meant the
// obvious value — what an HTML date input produces, and what the column actually
// stores — failed with `invalid_json`, an error that never mentions dates.
func TestCalendarDate_UnmarshalJSON_AcceptsBothForms(t *testing.T) {
	want := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"bare calendar date", `"2026-09-01"`},
		{"RFC3339 at UTC midnight", `"2026-09-01T00:00:00Z"`},
		{"RFC3339 with surrounding space", `"  2026-09-01  "`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got domain.CalendarDate
			if err := json.Unmarshal([]byte(tc.raw), &got); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !got.Time.Equal(want) {
				t.Fatalf("got %s, want %s", got.Time, want)
			}
			// UTC specifically: parsing a bare date in a zone behind Greenwich
			// would land on the previous day, so an invoice due on the 1st would
			// be stored as due on the 31st.
			if got.Time.Location() != time.UTC {
				t.Fatalf("expected UTC, got %s", got.Time.Location())
			}
		})
	}
}

// An RFC3339 value with a real offset keeps its instant, converted to UTC.
func TestCalendarDate_UnmarshalJSON_ConvertsOffsetToUTC(t *testing.T) {
	var got domain.CalendarDate
	if err := json.Unmarshal([]byte(`"2026-09-01T01:00:00+02:00"`), &got); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := time.Date(2026, 8, 31, 23, 0, 0, 0, time.UTC)
	if !got.Time.Equal(want) {
		t.Fatalf("got %s, want %s", got.Time, want)
	}
}

func TestCalendarDate_UnmarshalJSON_RejectsGarbageAndNamesTheField(t *testing.T) {
	for _, raw := range []string{`"not-a-date"`, `"01/09/2026"`, `"2026-13-45"`, `12345`, `true`} {
		var got domain.CalendarDate
		err := json.Unmarshal([]byte(raw), &got)
		if err == nil {
			t.Fatalf("expected %s to be refused", raw)
		}
		// The message is what reaches the caller as `detail`, and naming the
		// field is the entire improvement over "invalid character".
		if !strings.Contains(err.Error(), "due_date") {
			t.Fatalf("error for %s should name due_date, got: %v", raw, err)
		}
	}
}

// An empty string is left as the zero time so the required-field check — not the
// decoder — is what reports it, keeping "absent" and "malformed" apart.
func TestCalendarDate_UnmarshalJSON_EmptyStringIsZero(t *testing.T) {
	var got domain.CalendarDate
	if err := json.Unmarshal([]byte(`""`), &got); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Time.IsZero() {
		t.Fatalf("expected the zero time, got %s", got.Time)
	}
}

// The response shape must not change: only the accepted input widened.
func TestCalendarDate_MarshalJSON_StaysRFC3339(t *testing.T) {
	d := domain.CalendarDate{Time: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}

	out, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != `"2026-09-01T00:00:00Z"` {
		t.Fatalf("got %s, want RFC3339", out)
	}
}

// A round trip through the request struct must survive both input forms.
func TestCreateVendorInvoiceRequest_RoundTripsBareDate(t *testing.T) {
	body := `{"tenant_id":"t","legal_entity_id":"e","vendor_id":"v","invoice_number":"n",
	          "amount":1,"currency_code":"GBP","due_date":"2026-12-25","correlation_id":"c"}`

	var req domain.CreateVendorInvoiceRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.DueDate.IsZero() {
		t.Fatal("due_date should be set")
	}
	if y, m, d := req.DueDate.Date(); y != 2026 || m != time.December || d != 25 {
		t.Fatalf("got %04d-%02d-%02d, want 2026-12-25", y, m, d)
	}
}
