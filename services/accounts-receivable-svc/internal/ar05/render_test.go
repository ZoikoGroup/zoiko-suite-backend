package ar05

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

func testDoc() *SourceDocument {
	return SourceFromIssuedInvoice(sampleInvoice("11111111-1111-1111-1111-111111111111"))
}

func testProfile() *Profile {
	p := defaultProfile()
	p.Jurisdiction = "GB"
	p.EffectiveAt = testNow
	return &p
}

func TestJSONRendererIsDeterministic(t *testing.T) {
	doc, p := testDoc(), testProfile()
	first, err := JSONRenderer{}.Render(doc, p)
	require.NoError(t, err)
	for i := 0; i < 50; i++ {
		again, err := JSONRenderer{}.Render(doc, p)
		require.NoError(t, err)
		require.Equal(t, string(first), string(again))
	}
	require.Equal(t, Sha256Hex(first), Sha256Hex(append([]byte{}, first...)))

	// Shuffling the profile's mapping order does not change the bytes (keys are written sorted).
	q := *p
	q.SemanticMapping = append([]MappingEntry{}, p.SemanticMapping...)
	for i, j := 0, len(q.SemanticMapping)-1; i < j; i, j = i+1, j-1 {
		q.SemanticMapping[i], q.SemanticMapping[j] = q.SemanticMapping[j], q.SemanticMapping[i]
	}
	shuffled, err := JSONRenderer{}.Render(doc, &q)
	require.NoError(t, err)
	require.Equal(t, string(first), string(shuffled))

	// A different document gives different bytes.
	other := *doc
	other.Number = "INV-000043"
	diff, err := JSONRenderer{}.Render(&other, p)
	require.NoError(t, err)
	require.NotEqual(t, Sha256Hex(first), Sha256Hex(diff))
}

func TestJSONArtifactContent(t *testing.T) {
	doc, p := testDoc(), testProfile()
	b, err := JSONRenderer{}.Render(doc, p)
	require.NoError(t, err)
	var a jsonArtifact
	require.NoError(t, json.Unmarshal(b, &a))
	require.Equal(t, SchemaJSONArtifact, a.Meta.Schema)
	require.Equal(t, doc.ContentHash, a.Meta.ContentHash, "the artifact states the content hash it was rendered from")
	require.Equal(t, "PEPPOL_INVOICE", a.Meta.Profile)
	require.Equal(t, "INV-000042", a.Document["cbc:ID"])
	require.Equal(t, "1500.6000", a.Document["cbc:PayableAmount"], "money is the exact decimal string")
	require.Len(t, a.Lines, 2)
	require.Equal(t, "Platform licence", a.Lines[0]["cbc:Description"])
	require.Len(t, a.Meta.Dependencies, 2)
}

func TestValidatePasses(t *testing.T) {
	doc, p := testDoc(), testProfile()
	b, _ := JSONRenderer{}.Render(doc, p)
	rep := Validate(doc, p, b, Sha256Hex(b))
	require.True(t, rep.Passed, "%+v", rep.Issues)
	require.Empty(t, rep.Issues)
	require.NotEmpty(t, rep.Dependencies, "the schema / code-list versions in force are recorded")
}

func TestValidateFindings(t *testing.T) {
	has := func(rep ValidationReport, code string) bool {
		for _, i := range rep.Issues {
			if i.Code == code {
				return true
			}
		}
		return false
	}
	t.Run("unsupported canonical field", func(t *testing.T) {
		doc, p := testDoc(), testProfile()
		p.SemanticMapping = append(p.SemanticMapping, MappingEntry{From: "invoice.buyer.vat_id", To: "cbc:CompanyID"})
		b, _ := JSONRenderer{}.Render(doc, p)
		rep := Validate(doc, p, b, Sha256Hex(b))
		require.False(t, rep.Passed)
		require.True(t, has(rep, "REQUIRED_FIELD_UNSUPPORTED"))
	})
	t.Run("mandatory field not mapped", func(t *testing.T) {
		doc, p := testDoc(), testProfile()
		p.SemanticMapping = p.SemanticMapping[1:]
		b, _ := JSONRenderer{}.Render(doc, p)
		require.True(t, has(Validate(doc, p, b, Sha256Hex(b)), "FIELD_NOT_MAPPED"))
	})
	t.Run("totals do not reconcile", func(t *testing.T) {
		doc, p := testDoc(), testProfile()
		doc.Gross = kit.MustAmount("1500.61")
		b, _ := JSONRenderer{}.Render(doc, p)
		require.True(t, has(Validate(doc, p, b, Sha256Hex(b)), "TOTALS_DO_NOT_RECONCILE"))
	})
	t.Run("lines do not sum", func(t *testing.T) {
		doc, p := testDoc(), testProfile()
		doc.Lines[0].Net = kit.MustAmount("999.00")
		b, _ := JSONRenderer{}.Render(doc, p)
		require.True(t, has(Validate(doc, p, b, Sha256Hex(b)), "LINES_DO_NOT_SUM_TO_TOTALS"))
	})
	t.Run("tampered artifact", func(t *testing.T) {
		doc, p := testDoc(), testProfile()
		b, _ := JSONRenderer{}.Render(doc, p)
		stored := Sha256Hex(b)
		b[10] ^= 1
		require.True(t, has(Validate(doc, p, b, stored), "ARTIFACT_HASH_MISMATCH"))
	})
	t.Run("artifact for another document", func(t *testing.T) {
		doc, p := testDoc(), testProfile()
		other := *doc
		other.ContentHash = "deadbeef"
		b, _ := JSONRenderer{}.Render(&other, p)
		require.True(t, has(Validate(doc, p, b, Sha256Hex(b)), "ARTIFACT_PROFILE_MISMATCH"))
	})
	t.Run("document type mismatch", func(t *testing.T) {
		doc, p := testDoc(), testProfile()
		doc.DocumentType = DocCreditNote
		b, _ := JSONRenderer{}.Render(doc, p)
		require.True(t, has(Validate(doc, p, b, Sha256Hex(b)), "DOCUMENT_TYPE_MISMATCH"))
	})
	t.Run("no dependency in force", func(t *testing.T) {
		doc, p := testDoc(), testProfile()
		p.Dependencies = nil
		b, _ := JSONRenderer{}.Render(doc, p)
		require.True(t, has(Validate(doc, p, b, Sha256Hex(b)), "NO_DEPENDENCY_IN_FORCE"))
	})
	t.Run("no lines", func(t *testing.T) {
		doc, p := testDoc(), testProfile()
		doc.Lines = nil
		b, _ := JSONRenderer{}.Render(doc, p)
		require.True(t, has(Validate(doc, p, b, Sha256Hex(b)), "NO_LINES"))
	})
	t.Run("currency precision", func(t *testing.T) {
		doc, p := testDoc(), testProfile()
		doc.Currency = "JPY"
		b, _ := JSONRenderer{}.Render(doc, p)
		require.True(t, has(Validate(doc, p, b, Sha256Hex(b)), "AMOUNT_PRECISION"))
	})
}

func TestUnsupportedSyntaxIsRefusedNotApproximated(t *testing.T) {
	rs := DefaultRenderers()
	_, err := rs.For(SyntaxJSON)
	require.NoError(t, err)
	for _, syn := range []string{SyntaxUBL, SyntaxOther, "EDIFACT", ""} {
		_, err := rs.For(syn)
		var p *kit.Problem
		require.ErrorAs(t, err, &p, syn)
		require.Equal(t, kit.CodeEInvoiceValidationFailed, p.Code)
		require.Equal(t, 422, p.Status)
	}
}
