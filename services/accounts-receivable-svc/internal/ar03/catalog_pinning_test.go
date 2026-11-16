//go:build integration

package ar03

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/catalogref"
	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/svcclient"
)

type fakeCatalog struct {
	res    *catalogref.Result
	err    error
	calls  int
	caller svcclient.Caller
	offer  string
	vers   string
}

func (f *fakeCatalog) Check(_ context.Context, c svcclient.Caller, o, v string) (*catalogref.Result, error) {
	f.calls++
	f.caller, f.offer, f.vers = c, o, v
	return f.res, f.err
}

// BIZ-07 pinning: an order whose catalog_version_ref is catalog:<offering>:<version>
// cannot be approved unless that exact version is usable; anything else stays opaque.
func TestCatalogVersionPinning_CheckedAtApproval(t *testing.T) {
	e := newEnv(t)
	oid, vid := uuid.NewString(), uuid.NewString()
	ref := "catalog:" + oid + ":" + vid
	fc := &fakeCatalog{res: &catalogref.Result{Found: true, Usable: true}}
	e.m.WithCatalog(fc)

	// Opaque ref (the existing behaviour) never reaches the catalog.
	opaque := e.approved(nil)
	require.Equal(t, StateApproved, opaque.State)
	require.Zero(t, fc.calls, "an opaque catalog_version_ref must not be sent to the catalog")

	// A malformed catalog: ref is rejected when the order is written.
	b := createBody(e.entity, uuid.NewString())
	b["catalog_version_ref"] = "catalog:not-a-uuid"
	code, body := e.cmd(pCreator, "createSalesOrder", b)
	require.Equal(t, http.StatusBadRequest, code, string(body))

	o := e.submit(e.create(pCreator, map[string]any{"catalog_version_ref": ref}))
	approve := func() (int, []byte) { return e.mk(pApprover, "approveOrder", o, map[string]any{"version_no": 1}) }

	// Catalog unavailable: fail closed, order stays pending.
	fc.err = catalogref.ErrUnavailable
	code, body = approve()
	require.Equal(t, http.StatusServiceUnavailable, code, string(body))
	require.Equal(t, kit.CodeDependencyUnavailable, errCode(t, body))
	require.Equal(t, StatePendingApproval, e.get(o.OrderID).State)

	// Not wired at all: also fail closed.
	fc.err = nil
	e.m.catalog = nil
	code, _ = approve()
	require.Equal(t, http.StatusServiceUnavailable, code)
	e.m.WithCatalog(fc)

	// Unknown version, then an unusable one: CATALOG_VERSION_INVALID (422).
	fc.res = &catalogref.Result{Found: false}
	code, body = approve()
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeCatalogVersionInvalid, errCode(t, body))
	fc.res = &catalogref.Result{Found: true, Usable: false, Reason: "missing required mapping(s): TAX"}
	code, body = approve()
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	require.Equal(t, kit.CodeCatalogVersionInvalid, errCode(t, body))
	require.Equal(t, StatePendingApproval, e.get(o.OrderID).State)

	// A usable version approves; the catalog was asked about exactly that version
	// as the AR system principal for the right tenant.
	fc.res = &catalogref.Result{Found: true, Usable: true}
	o = e.approveAs(pApprover, o, nil)
	require.Equal(t, StateApproved, o.State)
	require.Equal(t, oid, fc.offer)
	require.Equal(t, vid, fc.vers)
	require.Equal(t, catalogref.SystemPrincipal, fc.caller.PrincipalID)
	require.Equal(t, e.tenant, fc.caller.TenantID)
}
