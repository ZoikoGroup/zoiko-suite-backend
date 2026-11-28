//go:build integration

package registryit_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/handler"
)

// newLedgerEnv is newEnvPolicy with decision capture on (DL-4).
func newLedgerEnv(signer domain.Signer, minReviews int) *env {
	pub := &spyPub{}
	auth := &recordingAuthz{}
	r := newRouter()
	h := handler.New(st, auth, pub, "platform-scope", zap.NewNop()).WithRegistry(st, pub).
		WithCertificationPolicy(minReviews).WithDecisionLedger(st.DB)
	if signer != nil {
		h.WithSigner(signer)
	}
	handler.RegisterRoutes(r, h)
	return &env{srv: r, pub: pub, auth: auth}
}

type ledgerRow struct {
	action, outcome, basis, onBehalf, tenant, entity string
}

func ledgerRowsFor(t *testing.T, basis string) []ledgerRow {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT record->>'action_type', record->>'outcome', record->>'rule_basis',
		record->'evaluation_context'->>'on_behalf_of', tenant_id, record->>'legal_entity_id'
		FROM decision_log_outbox WHERE record->>'rule_basis' = $1 ORDER BY created_at`, basis)
	require.NoError(t, err)
	defer rows.Close()
	var out []ledgerRow
	for rows.Next() {
		var r ledgerRow
		require.NoError(t, rows.Scan(&r.action, &r.outcome, &r.basis, &r.onBehalf, &r.tenant, &r.entity))
		out = append(out, r)
	}
	return out
}

// DL-4: certification is a governed decision. A refused certification writes
// no ledger record; a granted one writes exactly one, in the request's unit,
// recorded for the certifier with the pack version as its basis.
func TestDL4_CertificationQueuesOneLedgerRecordInTheUnit(t *testing.T) {
	c := signedReadyEnv(t, func(s domain.Signer) *env { return newLedgerEnv(s, 1) })
	basis := c.packRef + "@" + c.version

	code, r := c.e.do("POST", c.path("/certify"), "certifier", nil)
	mustStatus(t, 409, code, r)
	assert.Empty(t, ledgerRowsFor(t, basis), "a refused certification is not a decision the ledger records here")

	c.testsPassing(t)
	c.approve(t, "tax-reviewer", "tax")
	code, r = c.e.do("POST", c.path("/certify"), "certifier", nil)
	mustStatus(t, 201, code, r)

	got := ledgerRowsFor(t, basis)
	require.Len(t, got, 1)
	assert.Equal(t, ledgerRow{action: "JURISDICTION_PACK_CERTIFY", outcome: "CERTIFIED", basis: basis, onBehalf: "certifier",
		tenant: itTenant, entity: handler.LedgerPlatformScope}, got[0])

	// A repeat certify answers the stored certification and records nothing new.
	code, r = c.e.do("POST", c.path("/certify"), "certifier", nil)
	mustStatus(t, 200, code, r)
	assert.Len(t, ledgerRowsFor(t, basis), 1)
}
