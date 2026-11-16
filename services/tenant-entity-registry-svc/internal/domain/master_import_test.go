package domain_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

func rowsJSON(rows ...string) json.RawMessage {
	return json.RawMessage("[" + strings.Join(rows, ",") + "]")
}

const goodRow = `{"row_ref":"A-1","party_kind":"LEGAL_ENTITY","names":[{"type":"LEGAL_NAME","value":"Alpha Limited"}],"country":"GB","identifiers":[{"scheme":"LEI","value":"5493001KJTIIGC8Y1R12"}]}`

func goodUpload() domain.CreateMasterImportRequest {
	return domain.CreateMasterImportRequest{Kind: domain.ImportKindParty, SourceName: "crm", SourceVersion: "2026-10", EvidenceRef: "e", ValidFrom: "2026-01-01", Rows: rowsJSON(goodRow)}
}

func TestCreateMasterImportRequest_Validate(t *testing.T) {
	good := goodUpload()
	require.NoError(t, good.Validate())
	assert.Equal(t, domain.SourceSystemImport, good.SourceClassOrDefault())
	for name, mut := range map[string]func(*domain.CreateMasterImportRequest){
		"kind":        func(r *domain.CreateMasterImportRequest) { r.Kind = "LEGAL_ENTITY" },
		"source name": func(r *domain.CreateMasterImportRequest) { r.SourceName = " " },
		"version":     func(r *domain.CreateMasterImportRequest) { r.SourceVersion = "" },
		"evidence":    func(r *domain.CreateMasterImportRequest) { r.EvidenceRef = "" },
		"class":       func(r *domain.CreateMasterImportRequest) { r.SourceClass = "GUESS" },
		"valid from":  func(r *domain.CreateMasterImportRequest) { r.ValidFrom = "soon" },
		"no rows":     func(r *domain.CreateMasterImportRequest) { r.Rows = nil },
		"empty rows":  func(r *domain.CreateMasterImportRequest) { r.Rows = rowsJSON() },
		"not array":   func(r *domain.CreateMasterImportRequest) { r.Rows = json.RawMessage(`{"a":1}`) },
		"manifest":    func(r *domain.CreateMasterImportRequest) { r.ManifestSHA256 = "ABC" },
		"huge row": func(r *domain.CreateMasterImportRequest) {
			r.Rows = rowsJSON(`{"x":"` + strings.Repeat("a", domain.MaxImportRowSize) + `"}`)
		},
	} {
		r := goodUpload()
		mut(&r)
		assert.Error(t, r.Validate(), name)
	}
	many := make([]string, domain.MaxImportRows+1)
	for i := range many {
		many[i] = `{}`
	}
	r := goodUpload()
	r.Rows = rowsJSON(many...)
	assert.Error(t, r.Validate(), "too many rows")
}

func TestCreateMasterImportRequest_ManifestAndHashes(t *testing.T) {
	r := goodUpload()
	sum := r.ContentSHA256()
	require.Len(t, sum, 64)
	r.ManifestSHA256 = sum
	require.NoError(t, r.Validate(), "a matching manifest")
	r.ManifestSHA256 = strings.Repeat("0", 64)
	err := r.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match", "a damaged or changed file is refused")

	spaced := goodUpload()
	spaced.Rows = json.RawMessage("[\n  " + goodRow + "\n]")
	assert.Equal(t, goodUpload().ContentSHA256(), spaced.ContentSHA256(), "whitespace does not change the hash")
	other := goodUpload()
	other.Rows = rowsJSON(strings.Replace(goodRow, "Alpha", "Bravo", 1))
	assert.NotEqual(t, goodUpload().ContentSHA256(), other.ContentSHA256())

	assert.Equal(t, goodUpload().RequestHash(), goodUpload().RequestHash())
	for name, mut := range map[string]func(*domain.CreateMasterImportRequest){
		"rows":     func(r *domain.CreateMasterImportRequest) { r.Rows = other.Rows },
		"version":  func(r *domain.CreateMasterImportRequest) { r.SourceVersion = "2026-11" },
		"evidence": func(r *domain.CreateMasterImportRequest) { r.EvidenceRef = "other" },
		"valid":    func(r *domain.CreateMasterImportRequest) { r.ValidFrom = "2026-02-01" },
		"class":    func(r *domain.CreateMasterImportRequest) { r.SourceClass = domain.SourceTenantAsserted },
	} {
		x := goodUpload()
		mut(&x)
		assert.NotEqual(t, goodUpload().RequestHash(), x.RequestHash(), "a different request, so a retry with the same key is refused: "+name)
	}
}

func defaults() domain.ImportDefaults {
	return domain.ImportDefaults{ImportID: "imp1", SourceName: "crm", SourceClass: domain.SourceSystemImport, EvidenceRef: "e", ValidFrom: "2026-01-01"}
}

func TestValidateImportRows_EveryProblemOfEveryRowAtOnce(t *testing.T) {
	raws := [][]byte{
		[]byte(goodRow), // 1 valid
		[]byte(`{"row_ref":"A-2","party_kind":"BOGUS","names":[]}`), // 2 invalid party
		[]byte(`not json`), // 3 malformed
		[]byte(`{"row_ref":"A-1","party_kind":"LEGAL_ENTITY","names":[{"type":"LEGAL_NAME","value":"Dup Ref"}]}`),                                                                  // 4 duplicate ref
		[]byte(`{"row_ref":"A-5","party_kind":"LEGAL_ENTITY","names":[{"type":"LEGAL_NAME","value":"Same LEI"}],"identifiers":[{"scheme":"LEI","value":"5493001KJTIIGC8Y1R12"}]}`), // 5 duplicate identifier
		[]byte(`{"row_ref":"A-6","party_kind":"LEGAL_ENTITY","names":[{"type":"LEGAL_NAME","value":"X"}],"surprise":1}`),                                                           // 6 unknown field
		[]byte(`{"party_kind":"LEGAL_ENTITY","names":[{"type":"LEGAL_NAME","value":"No ref"}]}`),                                                                                   // 7 no row_ref
		[]byte(`{"row_ref":"A-8","party_kind":"NATURAL_PERSON","names":[{"type":"LEGAL_NAME","value":"Ada"}]}`),                                                                    // 8 valid
	}
	vs := domain.ValidateImportRows(raws, defaults())
	require.Len(t, vs, len(raws))
	want := []string{domain.RowValid, domain.RowInvalid, domain.RowInvalid, domain.RowInvalid, domain.RowInvalid, domain.RowInvalid, domain.RowInvalid, domain.RowValid}
	for i, v := range vs {
		assert.Equal(t, i+1, v.RowNo)
		assert.Equal(t, want[i], v.Status, "row %d: %+v", i+1, v.Issues)
	}
	assert.Equal(t, domain.IssueInvalid, vs[1].Issues[0].Code)
	assert.Equal(t, domain.IssueMalformed, vs[2].Issues[0].Code)
	assert.Equal(t, domain.IssueDuplicateRef, vs[3].Issues[0].Code)
	assert.Contains(t, vs[3].Issues[0].Message, "row 1")
	var codes []string
	for _, is := range vs[4].Issues {
		codes = append(codes, is.Code)
	}
	assert.Contains(t, codes, domain.IssueDuplicateIdentifier, "the first row to carry an identifier keeps it")
	assert.Equal(t, domain.IssueMalformed, vs[5].Issues[0].Code, "an unknown field is an error, not ignored")
	assert.Equal(t, domain.IssueRowRef, vs[6].Issues[0].Code)

	valid, invalid := domain.CountVerdicts(vs)
	assert.Equal(t, 2, valid)
	assert.Equal(t, 6, invalid)

	// Row references stay unique even for rows that had none or repeated one, so every row can be stored and found.
	seen := map[string]bool{}
	for _, v := range vs {
		assert.NotEmpty(t, v.RowRef)
		assert.False(t, seen[v.RowRef], "row_ref %s repeats", v.RowRef)
		seen[v.RowRef] = true
	}
	assert.Equal(t, "A-1", vs[0].RowRef)
	assert.Equal(t, "A-8", vs[7].RowRef)

	// One row alone is valid or not by itself; two rows never share an identifier across scopes that differ.
	two := domain.ValidateImportRows([][]byte{
		[]byte(`{"row_ref":"x","party_kind":"LEGAL_ENTITY","names":[{"type":"LEGAL_NAME","value":"One"}],"identifiers":[{"scheme":"COMPANY_REGISTRATION","scope":"GB","value":"123"}]}`),
		[]byte(`{"row_ref":"y","party_kind":"LEGAL_ENTITY","names":[{"type":"LEGAL_NAME","value":"Two"}],"identifiers":[{"scheme":"COMPANY_REGISTRATION","scope":"DE","value":"123"}]}`),
	}, defaults())
	assert.Equal(t, domain.RowValid, two[0].Status)
	assert.Equal(t, domain.RowValid, two[1].Status, "the same number in another country is another identifier")
}

func TestImportRowBecomesTheOrdinaryPartyRequest(t *testing.T) {
	row, err := domain.ParseImportRow([]byte(goodRow))
	require.NoError(t, err)
	req := row.PartyRequest(defaults(), 7)
	assert.Equal(t, "imp:imp1:7", req.CreateRequestID, "derived from the import and the row: running it again never makes a second party")
	assert.Equal(t, domain.CreateRequestIDFor("imp1", 7), req.CreateRequestID)
	assert.Equal(t, "crm", req.SourceSystem)
	assert.Equal(t, "A-1", req.SourceRef)
	assert.Equal(t, domain.SourceSystemImport, req.SourceClass)
	assert.Equal(t, "e", req.EvidenceRef)
	assert.Equal(t, "2026-01-01", req.ValidFrom)
	assert.NoError(t, req.Validate())
	row.ValidFrom = "2025-05-05"
	assert.Equal(t, "2025-05-05", row.PartyRequest(defaults(), 7).ValidFrom, "a row may say when its own facts began")

	_, err = domain.ParseImportRow([]byte(goodRow + ` {"x":1}`))
	assert.Error(t, err, "a row is one JSON object")
	_, err = domain.ParseImportRow([]byte(`{"row_ref":"a","create_request_id":"mine"}`))
	assert.Error(t, err, "a row cannot choose its own idempotency key")
}

func TestReconcile(t *testing.T) {
	ok := domain.Reconcile(10, 10, 2, 5, 2, 1, 0)
	assert.True(t, ok.Matched)
	assert.True(t, domain.Reconcile(3, 3, 3, 0, 0, 0, 0).Matched, "a file of only invalid rows accounts for itself")
	for name, r := range map[string]domain.Reconciliation{
		"rows lost":        domain.Reconcile(10, 9, 0, 9, 0, 0, 0),
		"unresolved":       domain.Reconcile(10, 10, 0, 8, 0, 0, 2),
		"double counted":   domain.Reconcile(10, 10, 0, 10, 1, 0, 0),
		"missing outcomes": domain.Reconcile(10, 10, 0, 8, 1, 0, 0),
	} {
		assert.False(t, r.Matched, name)
	}
}

func TestOutcomeOf_AnImportNeverOverwritesNorLinksOnItsOwn(t *testing.T) {
	created := domain.OutcomeOf(&domain.CreatePartyResult{Outcome: domain.OutcomeCreated, Party: &domain.Party{PartyID: "p1"}})
	assert.Equal(t, domain.RowImported, created.Status)
	assert.Equal(t, "p1", created.PartyID)
	matched := domain.OutcomeOf(&domain.CreatePartyResult{Outcome: domain.OutcomeMatchedExisting, Party: &domain.Party{PartyID: "p0"}})
	assert.Equal(t, domain.RowConflict, matched.Status, "an existing match is a conflict for stewardship, not an import")
	assert.Equal(t, "p0", matched.PartyID)
	review := domain.OutcomeOf(&domain.CreatePartyResult{Outcome: domain.OutcomeReviewRequired, Case: &domain.ResolutionCase{CaseID: "c1"}, Match: &domain.MatchCandidate{PartyID: "p9"}})
	assert.Equal(t, domain.RowConflict, review.Status)
	assert.Equal(t, "c1", review.CaseID)
	assert.Equal(t, "p9", review.PartyID)
	assert.Equal(t, domain.RowFailed, domain.OutcomeOf(nil).Status)
	assert.Equal(t, domain.RowConflict, domain.OutcomeOf(&domain.CreatePartyResult{Outcome: domain.OutcomeCreated}).Status, "created with no party named is not trusted as imported")
}

func TestApproveAndRowQueryValidation(t *testing.T) {
	assert.Error(t, domain.ApproveMasterImportRequest{}.Validate())
	assert.NoError(t, domain.ApproveMasterImportRequest{EvidenceRef: "e"}.Validate())
	assert.NoError(t, domain.ImportRowQuery{Status: domain.RowInvalid, AfterRow: 3, Limit: 200}.Validate())
	for _, q := range []domain.ImportRowQuery{{Status: "NOPE"}, {AfterRow: -1}, {Limit: 201}, {Limit: -1}} {
		assert.Error(t, q.Validate(), "%+v", q)
	}
}
