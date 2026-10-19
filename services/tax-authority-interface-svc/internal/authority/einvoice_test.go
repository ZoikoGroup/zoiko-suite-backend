package authority_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"zoiko.io/eventing/adapter"
	"zoiko.io/eventing/adapter/ext"
	"zoiko.io/tax-authority-interface-svc/internal/authority"
)

// ── an e-invoice network simulator: it receives UBL, not JSON ───────────────

type netSim struct {
	srv     *httptest.Server
	mu      sync.Mutex
	docs    map[string]string // document uuid -> receipt
	status  map[string]string
	bodies  []string
	ctypes  []string
	next    []string
	hang    chan struct{}
	submits int
}

func newNetSim(t *testing.T) *netSim {
	s := &netSim{docs: map[string]string{}, status: map[string]string{}, hang: make(chan struct{})}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(func() { close(s.hang); s.srv.Close() })
	return s
}

func (s *netSim) script(m ...string) { s.mu.Lock(); s.next = append(s.next, m...); s.mu.Unlock() }
func (s *netSim) setStatus(id, st string) {
	s.mu.Lock()
	s.status[id] = st
	s.mu.Unlock()
}
func (s *netSim) count() int { s.mu.Lock(); defer s.mu.Unlock(); return s.submits }

func (s *netSim) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		id := strings.TrimPrefix(r.URL.Path, "/v1/documents/by-uuid/")
		s.mu.Lock()
		rc, ok := s.docs[id]
		st := s.status[id]
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			return
		}
		if st == "" {
			st = "RECEIVED"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"document_id": rc, "status": st, "document_uuid": id})
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var doc struct {
		UUID string `xml:"UUID"`
	}
	_ = xml.Unmarshal(raw, &doc)
	s.mu.Lock()
	s.submits++
	s.bodies = append(s.bodies, string(raw))
	s.ctypes = append(s.ctypes, r.Header.Get("Content-Type"))
	mode := "ok"
	if len(s.next) > 0 {
		mode, s.next = s.next[0], s.next[1:]
	}
	commit := func() string {
		if rc, ok := s.docs[doc.UUID]; ok {
			return rc
		}
		rc := "DOC-" + doc.UUID
		s.docs[doc.UUID] = rc
		return rc
	}
	switch mode {
	case "ok":
		rc := commit()
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"document_id": rc, "status": "RECEIVED", "document_uuid": doc.UUID})
	case "commit-then-hang":
		commit()
		s.mu.Unlock()
		<-s.hang
	default:
		s.mu.Unlock()
		w.WriteHeader(500)
	}
}

func einvSpec(retrySafe bool) adapter.Spec {
	return adapter.Spec{
		Kind: "EINVOICE", RequiredScope: "einvoice.send", Rails: []string{"PEPPOL-BIS@3"}, Currencies: []string{"EUR"},
		Transform: adapter.TransformUBL,
		Submit:    adapter.Request{Path: "/v1/documents", ContentType: "application/xml"},
		Query: &adapter.Query{Method: "GET", Path: "/v1/documents/by-uuid/{idempotency_key}", NotFoundCodes: []int{404},
			Response: adapter.Response{RefPath: "document_id", StatusPath: "status", EchoPath: "document_uuid",
				StatusMap: map[string]string{"RECEIVED": "ACKNOWLEDGED", "CLEARED": "ACCEPTED", "DELIVERED": "DELIVERED", "REJECTED": "REJECTED"}}},
		Response: adapter.Response{RefPath: "document_id", StatusPath: "status", EchoPath: "document_uuid",
			StatusMap: map[string]string{"RECEIVED": "ACKNOWLEDGED", "CLEARED": "ACCEPTED", "DELIVERED": "DELIVERED", "REJECTED": "REJECTED"}},
		Auth: adapter.Auth{Type: "bearer"}, IdempotencyHeader: "Idempotency-Key", RetrySafe: retrySafe, TimeoutSeconds: 5,
		Schema:   json.RawMessage(`{"type":"object","required":["invoice_no","lines","totals"]}`),
		EInvoice: &adapter.EInvoiceRules{RecipientSchemes: map[string]string{"0088": `^[0-9]{13}$`, "0192": `^[0-9]{9}$`}, DocumentTypes: []string{"380", "381"}},
	}
}

const goodInv = `{"invoice_no":"INV-1","type":"380","issue_date":"2026-03-01","currency":"EUR",
"supplier":{"name":"Acme","endpoint":{"scheme":"0088","id":"4012345000009"}},"customer":{"name":"Buyer","endpoint":{"scheme":"0192","id":"987654321"}},
"lines":[{"id":"1","description":"Widget","quantity":"3","unit_price":"10.00","tax_rate":"20.00","line_total":"30.00"}],
"totals":{"net":"30.00","tax":"6.00","gross":"36.00"}}`

type einvEnv struct {
	t     *testing.T
	sim   *netSim
	st    *fakeStore
	eng   *authority.Engine
	creds *fakeCreds
	n     int
}

func newEinvEnv(t *testing.T, retrySafe bool) *einvEnv {
	sm := newNetSim(t)
	st := newFakeStore()
	_, _ = st.PutProfile(context.Background(), "einvnet", einvSpec(retrySafe), "author")
	creds := &fakeCreds{}
	e := &einvEnv{t: t, sim: sm, st: st, creds: creds}
	e.eng = &authority.Engine{Store: st, Creds: creds, Transport: &adapter.Transport{Client: &http.Client{Timeout: 400 * time.Millisecond}},
		Connectors: &fakeConns{c: ext.Connector{ID: "c1", LegalEntityID: entity, EndpointURL: sm.srv.URL, Status: "ACTIVE", CredentialRef: "pol",
			Scopes: []string{"einvoice.send"}, ConfigFingerprint: "fp", ApprovedFingerprint: "fp"}},
		Cfg: authority.Config{BreakerFailures: 3, PollInterval: time.Minute, UrgentPollInterval: 10 * time.Second},
		Now: func() time.Time { return st.now() }}
	return e
}

func (e *einvEnv) filing(doc string) string {
	e.n++
	id := "inv-filing-" + string(rune('a'+e.n))
	e.st.f[id] = &authority.Filing{SubmissionID: id, TenantID: "t1", InterfaceID: "if1", LegalEntityID: entity, Jurisdiction: "DE", AdapterKind: authority.KindEInvoice,
		ProviderKey: "einvnet", ConnectorID: "c1", EscalateBefore: 24 * time.Hour, TaxPeriod: "2026-03", FilingType: "INVOICE", AmountText: "36.0000",
		SubjectRef: "subj-" + id, Document: []byte(doc), Rail: "PEPPOL-BIS@3", Status: authority.StatusPrepared, SubmittedAt: time.Now()}
	return id
}

func (e *einvEnv) send(id string) (*authority.Result, error) {
	return e.eng.Send(context.Background(), id, "op", ext.Caller{TenantID: "t1", PrincipalID: "op"})
}
func (e *einvEnv) poll(id string) (*authority.Result, error) {
	return e.eng.Poll(context.Background(), id, "op", ext.Caller{TenantID: "t1", PrincipalID: "op"})
}
func (e *einvEnv) status(id string) string { return e.st.f[id].Status }

func TestEInvoiceIsTransformedSentAndFollowedToDelivery(t *testing.T) {
	e := newEinvEnv(t, true)
	id := e.filing(goodInv)
	res, err := e.send(id)
	if err != nil || res.Filing.Status != authority.StatusSubmitted {
		t.Fatalf("%+v %v", res, err)
	}
	if e.sim.ctypes[0] != "application/xml" {
		t.Fatalf("the network receives XML: %s", e.sim.ctypes[0])
	}
	body := e.sim.bodies[0]
	for _, want := range []string{`<Invoice xmlns="urn:oasis:names:specification:ubl:schema:xsd:Invoice-2"`, `<cbc:UUID>` + id + `</cbc:UUID>`, `<cbc:PayableAmount currencyID="EUR">36.00</cbc:PayableAmount>`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in\n%s", want, body)
		}
	}
	f := e.st.f[id]
	if f.SentPayloadHash != adapter.HashBytes([]byte(body)) || f.ProfileVersion == nil || *f.ProfileVersion != 1 {
		t.Fatalf("the exact transformed payload is pinned: %+v", f)
	}
	log, _ := e.st.ListTransport(context.Background(), id)
	if len(log) != 1 || log[0].RequestHash != f.SentPayloadHash {
		t.Fatalf("the evidence hash is the transformed payload's: %+v", log)
	}

	// received is not cleared; clearing is not delivery; delivery is final
	e.sim.setStatus(id, "CLEARED")
	res, err = e.poll(id)
	if err != nil || res.Filing.Status != authority.StatusAccepted || res.Filing.NextPollAt == nil {
		t.Fatalf("cleared, and still being asked about: %+v %v", res, err)
	}
	if res, err = e.poll(id); err != nil || res.Filing.Status != authority.StatusAccepted || res.Note != "accepted_awaiting_delivery" {
		t.Fatalf("%+v %v", res, err)
	}
	e.sim.setStatus(id, "DELIVERED")
	res, err = e.poll(id)
	if err != nil || res.Filing.Status != authority.StatusDelivered {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := e.poll(id); !errors.Is(err, authority.ErrInFlight) {
		t.Fatalf("a delivered document is not polled: %v", err)
	}
	if e.sim.count() != 1 {
		t.Fatalf("one document sent: %d", e.sim.count())
	}
}

// acceptance: wrong profile / recipient invalid / attachment prohibited / inconsistent economics — all block before send
func TestRefusalsBlockBeforeSend(t *testing.T) {
	pdf := base64.StdEncoding.EncodeToString([]byte("%PDF"))
	cases := map[string]func(*einvEnv, string){
		"wrong profile (unsupported rail)": func(e *einvEnv, id string) { e.st.f[id].Rail = "OTHER-NET@1" },
		"recipient scheme unlisted": func(e *einvEnv, id string) {
			e.st.f[id].Document = []byte(strings.Replace(goodInv, `"0192"`, `"9999"`, 1))
		},
		"recipient id invalid": func(e *einvEnv, id string) {
			e.st.f[id].Document = []byte(strings.Replace(goodInv, `987654321`, `12`, 1))
		},
		"attachment prohibited": func(e *einvEnv, id string) {
			e.st.f[id].Document = []byte(strings.Replace(goodInv, `"totals"`, `"attachments":[{"filename":"a.pdf","mime":"application/pdf","content_base64":"`+pdf+`"}],"totals"`, 1))
		},
		"total does not add up": func(e *einvEnv, id string) {
			e.st.f[id].Document = []byte(strings.Replace(goodInv, `"gross":"36.00"`, `"gross":"37.00"`, 1))
		},
		"missing required": func(e *einvEnv, id string) { e.st.f[id].Document = []byte(`{"invoice_no":"X"}`) },
	}
	for name, mut := range cases {
		e := newEinvEnv(t, true)
		id := e.filing(goodInv)
		mut(e, id)
		res, err := e.send(id)
		if !errors.Is(err, authority.ErrBlocked) || res == nil || res.Filing.Status != authority.StatusBlocked {
			t.Errorf("%s: %+v %v", name, res, err)
		}
		if e.sim.count() != 0 {
			t.Errorf("%s: the network was called", name)
		}
		// BLOCKED is final: the document is never adapted to pass
		if _, err := e.send(id); !errors.Is(err, authority.ErrBlocked) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// no value is echoed
	e := newEinvEnv(t, true)
	id := e.filing(strings.Replace(goodInv, `987654321`, `SECRET-ID`, 1))
	res, _ := e.send(id)
	if res == nil || strings.Contains(res.Note, "SECRET-ID") || strings.Contains(e.st.blocked[id], "SECRET-ID") {
		t.Fatalf("a value leaked into the failure: %+v", res)
	}
}

// acceptance: network timeout, duplicate send
func TestTimeoutThenRetryNeverSendsTheInvoiceTwice(t *testing.T) {
	e := newEinvEnv(t, true)
	id := e.filing(goodInv)
	e.sim.script("commit-then-hang")
	if _, err := e.send(id); err != nil {
		t.Fatal(err)
	}
	if e.status(id) != authority.StatusPendingUnknown {
		t.Fatalf("%s", e.status(id))
	}
	res, err := e.send(id)
	if err != nil || res.Filing.Status != authority.StatusSubmitted || res.Note != "found_at_authority" {
		t.Fatalf("%+v %v", res, err)
	}
	if e.sim.count() != 1 {
		t.Fatalf("the network received the invoice %d times", e.sim.count())
	}
	// a duplicate send of a submitted document is refused outright
	if _, err := e.send(id); !errors.Is(err, authority.ErrInFlight) {
		t.Fatalf("%v", err)
	}
	if e.sim.count() != 1 {
		t.Fatal("still one")
	}
}

// acceptance: profile update after approval
func TestAProfileChangedAfterValidationNeverReTransformsAnApprovedDocument(t *testing.T) {
	e := newEinvEnv(t, true)
	id := e.filing(goodInv)
	if res, err := e.eng.Validate(context.Background(), id, "op", "c"); err != nil || res.Filing.Status != authority.StatusValidated {
		t.Fatalf("%+v %v", res, err)
	}
	if v := e.st.f[id].ProfileVersion; v == nil || *v != 1 {
		t.Fatalf("validation pins the profile version: %v", v)
	}
	id3 := e.filing(goodInv)
	if _, err := e.eng.Validate(context.Background(), id3, "op", "c"); err != nil {
		t.Fatal(err)
	}
	// the network publishes v2: it no longer accepts the customer's scheme
	v2 := einvSpec(true)
	v2.EInvoice.RecipientSchemes = map[string]string{"0088": `^[0-9]{13}$`}
	_, _ = e.st.PutProfile(context.Background(), "einvnet", v2, "author")
	res, err := e.send(id)
	if err != nil || res.Filing.Status != authority.StatusSubmitted {
		t.Fatalf("the filing keeps the profile it was validated under: %+v %v", res, err)
	}
	log, _ := e.st.ListTransport(context.Background(), id)
	if len(log) != 1 || log[0].ProfileVersion != 1 {
		t.Fatalf("sent under v1: %+v", log)
	}
	// a NEW filing (a new subject) validates and sends under the new profile, and is refused by it
	id2 := e.filing(goodInv)
	res, err = e.send(id2)
	if !errors.Is(err, authority.ErrBlocked) || res.Filing.Status != authority.StatusBlocked {
		t.Fatalf("a new approval under the new profile: %+v %v", res, err)
	}
	// a profile found unsafe and withdrawn stops even a validated filing
	e.st.profiles[0].Status = authority.ProfileWithdrawn
	before := e.sim.count()
	if _, err := e.send(id3); !errors.Is(err, authority.ErrProfileNotFound) {
		t.Fatalf("%v", err)
	}
	if e.sim.count() != before {
		t.Fatal("nothing may be sent under a withdrawn profile")
	}
}

func TestAnInterfaceKindMustMatchTheProfileKind(t *testing.T) {
	e := newEinvEnv(t, true)
	// a TAX profile cannot carry an e-invoice, and an e-invoice profile cannot carry a tax filing
	e.st.profiles = nil
	_, _ = e.st.PutProfile(context.Background(), "einvnet", taxSpec(true), "a")
	id := e.filing(goodInv)
	if _, err := e.send(id); !errors.Is(err, authority.ErrBlocked) {
		t.Fatalf("%v", err)
	}
	if err := e.eng.CheckBinding(context.Background(), "einvnet", "VAT-RETURN@2", authority.KindEInvoice); !errors.Is(err, authority.ErrProfileNotFound) {
		t.Fatalf("%v", err)
	}
	if e.sim.count() != 0 {
		t.Fatal("nothing sent")
	}
}
