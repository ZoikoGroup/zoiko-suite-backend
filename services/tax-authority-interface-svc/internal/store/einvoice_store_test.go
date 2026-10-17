package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/eventing/adapter"
	"zoiko.io/tax-authority-interface-svc/internal/authority"
)

func (f *fixture) einvoiceIface(tenant string) string {
	f.t.Helper()
	id := f.iface(tenant, nil)
	_, err := f.admin.Exec(context.Background(), `UPDATE tax_interfaces SET adapter_kind = 'EINVOICE' WHERE interface_id = $1`, id)
	require.NoError(f.t, err)
	return id
}

func einvSpecForStore() adapter.Spec {
	return adapter.Spec{
		Kind: "EINVOICE", RequiredScope: "einvoice.send", Rails: []string{"PEPPOL-BIS@3"}, Transform: adapter.TransformUBL,
		Submit:   adapter.Request{Path: "/v1/documents", ContentType: "application/xml"},
		Response: adapter.Response{RefPath: "document_id", StatusPath: "status", StatusMap: map[string]string{"RECEIVED": "ACKNOWLEDGED", "DELIVERED": "DELIVERED"}},
		Auth:     adapter.Auth{Type: "bearer"}, TimeoutSeconds: 5, Schema: json.RawMessage(`{"type":"object"}`),
		EInvoice: &adapter.EInvoiceRules{RecipientSchemes: map[string]string{"0088": `^[0-9]{13}$`}},
	}
}

func TestEInvoice_InterfaceKindAndProfilesAreStored(t *testing.T) {
	f := newFixture(t)
	key := "einv-" + uuid.NewString()[:8]
	p, err := f.st.PutProfile(context.Background(), key, einvSpecForStore(), "author")
	require.NoError(t, err)
	assert.Equal(t, "EINVOICE", p.Spec.Kind)
	assert.Equal(t, adapter.TransformUBL, p.Spec.Transform)
	require.NotNil(t, p.Spec.EInvoice)
	tenant := uuid.NewString()
	fl := f.filing(tenant, f.einvoiceIface(tenant))
	assert.Equal(t, authority.KindEInvoice, fl.AdapterKind)
	assert.Equal(t, authority.KindTax, f.filing(tenant, f.iface(tenant, nil)).AdapterKind, "interfaces carry tax by default")
	_, err = f.admin.Exec(context.Background(), `UPDATE tax_interfaces SET adapter_kind = 'OTHER' WHERE interface_id = $1`, fl.InterfaceID)
	assert.Error(t, err, "only TAX and EINVOICE")
}

func TestEInvoice_ValidationPinsTheProfileVersion(t *testing.T) {
	f := newFixture(t)
	tenant := uuid.NewString()
	ctx := tc(tenant)
	fl := f.filing(tenant, f.einvoiceIface(tenant))
	v, err := f.st.MarkValidated(ctx, fl.SubmissionID, "op", 3)
	require.NoError(t, err)
	require.NotNil(t, v.ProfileVersion)
	assert.Equal(t, 3, *v.ProfileVersion, "validation pins the profile version the document was approved under")
	// the version the filing was validated under is the only one it can be sent with
	_, err = f.st.BeginSend(ctx, fl.SubmissionID, authority.StatusValidated, 4, "h", "op")
	assert.ErrorIs(t, err, authority.ErrPayloadChanged)
	_, err = f.admin.Exec(context.Background(), `UPDATE tax_filing_submissions SET profile_version = 4 WHERE submission_id = $1`, fl.SubmissionID)
	assert.Error(t, err)
	_, err = f.st.BeginSend(ctx, fl.SubmissionID, authority.StatusValidated, 3, "h", "op")
	assert.NoError(t, err)
}

func TestEInvoice_AcceptedThenDeliveredIsTheOnlyForwardMove(t *testing.T) {
	f := newFixture(t)
	tenant := uuid.NewString()
	ctx := tc(tenant)
	fl := f.filing(tenant, f.einvoiceIface(tenant))
	_, _ = f.st.MarkValidated(ctx, fl.SubmissionID, "op", 1)
	_, err := f.st.BeginSend(ctx, fl.SubmissionID, authority.StatusValidated, 1, "h", "op")
	require.NoError(t, err)
	next := time.Now().Add(time.Minute)
	_, err = f.st.Finish(ctx, fl.SubmissionID, authority.Outcome{Kind: authority.OutSubmitted, From: authority.StatusSubmitting, AuthorityRef: "DOC-1", NextPoll: &next,
		Transport: entry("SUBMIT", "ANSWERED", "ACKNOWLEDGED")}, "op", "c")
	require.NoError(t, err)
	got, err := f.st.Finish(ctx, fl.SubmissionID, authority.Outcome{Kind: authority.OutAccepted, From: authority.StatusSubmitted, AuthorityRef: "DOC-1", Authority: "CLEARED", NextPoll: &next,
		Transport: entry("QUERY", "ANSWERED", "ACCEPTED")}, "op", "c")
	require.NoError(t, err)
	assert.Equal(t, authority.StatusAccepted, got.Status)
	assert.NotNil(t, got.NextPollAt, "an accepted e-invoice keeps being asked about until it is delivered")
	assert.Equal(t, 1, f.outbox(tenant, fl.SubmissionID, "einvoice.network-attempt.accepted"))
	assert.Equal(t, 0, f.outbox(tenant, fl.SubmissionID, "authority-attempt.response-received"), "e-invoice events are their own contract")

	// accepted cannot become rejected, or be re-sent, but it can be delivered
	su := func(sql string) error { _, err := f.admin.Exec(context.Background(), sql, fl.SubmissionID); return err }
	assert.Error(t, su(`UPDATE tax_filing_submissions SET status = 'REJECTED' WHERE submission_id = $1`))
	assert.Error(t, su(`UPDATE tax_filing_submissions SET status = 'PENDING_UNKNOWN' WHERE submission_id = $1`))
	due, err := f.st.DuePolls(context.Background(), time.Now().Add(2*time.Minute), 50)
	require.NoError(t, err)
	found := false
	for _, d := range due {
		found = found || d.SubmissionID == fl.SubmissionID
	}
	assert.True(t, found, "an accepted e-invoice is due for polling")

	d, err := f.st.Finish(ctx, fl.SubmissionID, authority.Outcome{Kind: authority.OutDelivered, From: authority.StatusAccepted, AuthorityRef: "DOC-1", Authority: "DELIVERED",
		Transport: entry("QUERY", "ANSWERED", "DELIVERED")}, "op", "c")
	require.NoError(t, err)
	assert.Equal(t, authority.StatusDelivered, d.Status)
	assert.Nil(t, d.NextPollAt)
	assert.Equal(t, 1, f.outbox(tenant, fl.SubmissionID, "einvoice.network-attempt.delivered"))
	assert.Equal(t, "DOC-1", f.ack(fl.SubmissionID))
	assert.Error(t, su(`UPDATE tax_filing_submissions SET status = 'ACCEPTED' WHERE submission_id = $1`), "delivered is final")
	assert.Error(t, su(`UPDATE tax_filing_submissions SET authority_ref = 'other' WHERE submission_id = $1`))
}

func TestEInvoice_DeliveryIsNotATaxFilingState(t *testing.T) {
	f := newFixture(t)
	tenant := uuid.NewString()
	ctx := tc(tenant)
	fl := f.sentFiling(tenant, false) // a TAX filing
	_, err := f.admin.Exec(context.Background(), `UPDATE tax_filing_submissions SET status = 'DELIVERED' WHERE submission_id = $1`, fl.SubmissionID)
	assert.Error(t, err, "the database refuses DELIVERED for a tax filing")
	_, err = f.st.Finish(ctx, fl.SubmissionID, authority.Outcome{Kind: authority.OutDelivered, From: authority.StatusSubmitted, Transport: entry("QUERY", "ANSWERED", "DELIVERED")}, "op", "c")
	assert.Error(t, err)
	res, err := f.st.ApplyCallback(ctx, cbIn("D1", fl.AuthorityRef, "DLV", "DELIVERED"))
	require.NoError(t, err)
	assert.Equal(t, authority.OutcomeStaleIgnored, res.Outcome)
	cur, _ := f.st.LoadFiling(ctx, fl.SubmissionID)
	assert.Equal(t, authority.StatusSubmitted, cur.Status)
	// and a tax acceptance is still final
	res, _ = f.st.ApplyCallback(ctx, cbIn("D2", fl.AuthorityRef, "ACC", "ACCEPTED"))
	assert.Equal(t, authority.OutcomeApplied, res.Outcome)
	res, _ = f.st.ApplyCallback(ctx, cbIn("D3", fl.AuthorityRef, "ACC2", "REJECTED"))
	assert.Equal(t, authority.OutcomeRegressionBlocked, res.Outcome)
}

func TestEInvoice_CallbacksTakeAnAcceptedDocumentToDelivered(t *testing.T) {
	f := newFixture(t)
	tenant := uuid.NewString()
	ctx := tc(tenant)
	fl := f.filing(tenant, f.einvoiceIface(tenant))
	_, _ = f.st.MarkValidated(ctx, fl.SubmissionID, "op", 1)
	_, _ = f.st.BeginSend(ctx, fl.SubmissionID, authority.StatusValidated, 1, "h", "op")
	_, err := f.st.Finish(ctx, fl.SubmissionID, authority.Outcome{Kind: authority.OutSubmitted, From: authority.StatusSubmitting, AuthorityRef: "DOC-9", Transport: entry("SUBMIT", "ANSWERED", "ACKNOWLEDGED")}, "op", "c")
	require.NoError(t, err)

	res, _ := f.st.ApplyCallback(ctx, cbIn("N1", "DOC-9", "CLR", "ACCEPTED"))
	assert.Equal(t, authority.OutcomeApplied, res.Outcome)
	assert.Equal(t, authority.StatusAccepted, res.Filing.Status)
	res, _ = f.st.ApplyCallback(ctx, cbIn("N2", "DOC-9", "PRC", "PENDING"))
	assert.Equal(t, authority.OutcomeStaleIgnored, res.Outcome, "an earlier stage never moves it back")
	res, _ = f.st.ApplyCallback(ctx, cbIn("N3", "DOC-9", "DLV", "DELIVERED"))
	assert.Equal(t, authority.OutcomeApplied, res.Outcome)
	assert.Equal(t, authority.StatusDelivered, res.Filing.Status)
	res, _ = f.st.ApplyCallback(ctx, cbIn("N4", "DOC-9", "CLR", "ACCEPTED"))
	assert.Equal(t, authority.OutcomeRegressionBlocked, res.Outcome, "a delivered document is final")
	res, _ = f.st.ApplyCallback(ctx, cbIn("N3", "DOC-9", "DLV", "DELIVERED"))
	assert.Equal(t, authority.OutcomeDuplicate, res.Outcome)
	assert.Equal(t, 1, f.outbox(tenant, fl.SubmissionID, "einvoice.network-attempt.delivered"))
}
