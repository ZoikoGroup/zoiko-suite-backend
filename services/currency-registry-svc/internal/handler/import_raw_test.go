package handler_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/currency-registry-svc/internal/domain"
)

func TestImport_OmittedMinorUnitQuarantines(t *testing.T) {
	e := newEnv(t)
	rows := []domain.ImportRow{{AlphaCode: "AAA", NumericCode: "100", Name: "No exponent"}} // minor_unit omitted
	raw := fmt.Sprintf(`{"source_name":"s","source_version":"1","manifest_hash":%q,"reason":"r","rows":[{"alpha_code":"AAA","numeric_code":"100","name":"No exponent"}]}`,
		domain.ManifestHash(rows))
	rec := e.do("POST", "/v1/currency-imports", raw)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	imp := decode[domain.Import](t, rec)
	assert.Equal(t, domain.ImportQuarantined, imp.Status)
	assert.Empty(t, e.store.Currencies())
}
