package jurisdiction_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/jurisdiction"
)

// Before 28 Sep 2026 any 200 from jurisdiction-rules-svc counted as valid,
// so a deactivated or expired jurisdiction was accepted for new records.
func TestHTTPValidator_RefusesRetiredAndInactiveJurisdictions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/v1/jurisdictions/") {
		case "live":
			_, _ = w.Write([]byte(`{"jurisdiction_id":"live","jurisdiction_code":"GB","active_flag":true,"effective_to":null}`))
		case "inactive":
			_, _ = w.Write([]byte(`{"jurisdiction_id":"inactive","jurisdiction_code":"XX","active_flag":false}`))
		case "expired":
			_, _ = w.Write([]byte(`{"jurisdiction_id":"expired","jurisdiction_code":"YU","active_flag":true,"effective_to":"2003-02-04T00:00:00Z"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	v := jurisdiction.NewHTTPValidator(srv.URL, zap.NewNop())
	ctx := context.Background()

	if err := v.ValidateExists(ctx, "live"); err != nil {
		t.Fatalf("live: %v", err)
	}
	for _, id := range []string{"inactive", "expired"} {
		if err := v.ValidateExists(ctx, id); !errors.Is(err, jurisdiction.ErrJurisdictionRetired) {
			t.Errorf("%s: got %v, want ErrJurisdictionRetired", id, err)
		}
	}
	if err := v.ValidateExists(ctx, "nope"); !errors.Is(err, jurisdiction.ErrJurisdictionNotFound) {
		t.Errorf("unknown: got %v", err)
	}
	ref, err := v.Lookup(ctx, "live")
	if err != nil || ref.JurisdictionCode != "GB" {
		t.Errorf("lookup: %+v %v", ref, err)
	}
}
