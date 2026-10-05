package handler_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Every /v1/admin/* write requires its iam.* permission. No test asserted
// this gate before: the tests that exercised these routes did so as a caller
// the stub never granted anything, failed with 403 from the day the gate was
// added, and nobody saw because the package did not compile.
func TestAdminWritesRequireTheirPermission(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	cases := map[string]string{
		"/v1/admin/roles":                            `{}`,
		"/v1/admin/roles/r-1/retire":                 `{}`,
		"/v1/admin/roles/r-1/reactivate":             `{}`,
		"/v1/admin/roles/r-1/permission-bundles":     `{}`,
		"/v1/admin/permission-bundles/b-1/retire":    `{}`,
		"/v1/admin/role-assignments":                 `{}`,
		"/v1/admin/role-assignments/a-1/revoke":      `{}`,
		"/v1/admin/delegated-authorities":            `{}`,
		"/v1/admin/delegated-authorities/d-1/revoke": `{}`,
		"/v1/admin/sod-rules/s-1/retire":             `{}`,
		"/v1/admin/abac-rules/x-1/retire":            `{}`,
		"/v1/admin/privileged-sessions":              `{}`,
		"/v1/admin/privileged-sessions/p-1/revoke":   `{}`,
		"/v1/admin/break-glass-sessions":             `{}`,
		"/v1/admin/break-glass-sessions/b-1/revoke":  `{}`,
		"/v1/admin/sod-rules":                        `{"tenant_id":"` + tenant + `","domain_code":"PAYMENTS","action_a":"payment.approve","action_b":"payment.release","conflict_type":"STATIC"}`,
	}
	for path, body := range cases {
		r := newTestRouter(&stubStore{denyAdmin: true})
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
		req.Header.Set("X-Principal-Id", "clerk-1")
		req.Header.Set("X-Tenant-Id", tenant)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s by a non-administrator: want 403, got %d: %s", path, w.Code, w.Body.String())
		}
	}
}
