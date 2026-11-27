package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/general-ledger-svc/internal/certreliance"
)

// ZS-SVC-Z-001 RB-05 / DQC-017: when a certification the ledger relied on is revoked, the ledger finds out, flags every posted journal
// and kernel posting that cites it, and a person acknowledges each flag. Against real Postgres, through the real routes.

type fakeStatusService struct {
	mu    sync.Mutex
	by    map[string]certreliance.Status
	errs  map[string]error
	asked []string
}

func newFakeStatusService() *fakeStatusService {
	return &fakeStatusService{by: map[string]certreliance.Status{}, errs: map[string]error{}}
}

func (f *fakeStatusService) CertificationStatus(_ context.Context, _, _, _ string, ref certreliance.Ref) (certreliance.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, ref.String())
	if err := f.errs[ref.String()]; err != nil {
		return certreliance.Status{}, err
	}
	if st, ok := f.by[ref.String()]; ok {
		return st, nil
	}
	return certreliance.Status{State: certreliance.StateActive}, nil
}

func (f *fakeStatusService) set(ref string, st certreliance.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.by[ref] = st
}

const (
	certX = "certification:data-quality-svc:dqn_x"
	certY = "certification:data-quality-svc:dqn_y"
	certZ = "certification:reconciliation-engine-svc:rcr_z"
)

var (
	revokedX = certreliance.Status{State: certreliance.StateRevoked, RevocationID: "dqx_1", Reason: "false pass: the amount rule never read amended rows"}
	revokedZ = certreliance.Status{State: certreliance.StateRevoked, RevocationID: "rcv_2", Reason: "bank statement restated after certification"}
)

// postManual takes a manual journal that cites the given evidence all the way to FINALIZED through the real routes.
func (e *kenv) postManual(t *testing.T, corr string, evidence ...string) string {
	t.Helper()
	id := e.mustCreate(t, e.manualBody(corr, "ADJUSTMENT", "ACCRUAL", 50, evidence...))
	if st, b := e.call("POST", "/v1/journals/"+id+"/validate", e.tenant, "preparer", map[string]any{}); st != 200 {
		t.Fatalf("validate: %d %v", st, b)
	}
	if st, b := e.call("POST", "/v1/journals/"+id+"/post", e.tenant, "preparer", map[string]any{}); st != 200 {
		t.Fatalf("post: %d %v", st, b)
	}
	return id
}

func (e *kenv) sweep(t *testing.T, entity string) map[string]any {
	t.Helper()
	st, b := e.call("POST", "/v1/certification-watch/sweeps", e.tenant, "sweeper", map[string]any{"legal_entity_id": entity})
	if st != 200 {
		t.Fatalf("sweep: %d %v", st, b)
	}
	return b
}

func (e *kenv) flags(t *testing.T, entity, status string) []map[string]any {
	t.Helper()
	path := "/v1/certification-flags?legal_entity_id=" + entity
	if status != "" {
		path += "&status=" + status
	}
	st, b := e.call("GET", path, e.tenant, "viewer", nil)
	if st != 200 {
		t.Fatalf("flags: %d %v", st, b)
	}
	var out []map[string]any
	for _, f := range b["data"].([]any) {
		out = append(out, f.(map[string]any))
	}
	return out
}

func TestCertWatch_RevokedCertificationFlagsExactlyWhatRestsOnIt(t *testing.T) {
	e := newKEnv(t)
	book, _ := e.postSetup(t)
	e.svc.Reliance = e.recorder

	jX := e.postManual(t, "w-1", "doc-1", certX)    // rests on a certification that will be revoked
	jY := e.postManual(t, "w-2", certY)             // rests on one that stays in force
	jNone := e.postManual(t, "w-3", "doc-2")        // cites no certification
	evZ := e.mustRegisterCiting(t, "se-w-z", certZ) // a kernel posting resting on a certification that will be revoked
	resZ := e.mustPost(t, evZ, book)
	e.mustPost(t, e.registerEvent(t, "se-w-none", "120.00", "100.00", "20.00"), book)

	// Posting put each certification on the watch list; nothing is withdrawn yet.
	watched, err := e.watch.ListWatched(e.ctx(), e.tenant, e.entity)
	if err != nil || len(watched) != 3 {
		t.Fatalf("watched: %+v, %v", watched, err)
	}
	for _, w := range watched {
		if w.WithdrawnAt != nil || w.LastStatus != "" {
			t.Fatalf("a certification was withdrawn or checked before any sweep: %+v", w)
		}
	}

	e.status.set(certX, revokedX)
	e.status.set(certZ, revokedZ)
	res := e.sweep(t, e.entity)
	if res["checked"] != float64(3) || res["withdrawn"] != float64(2) || res["new_flags"] != float64(2) || res["failed"] != float64(0) {
		t.Fatalf("sweep: %v", res)
	}

	flags := e.flags(t, e.entity, "OPEN")
	if len(flags) != 2 {
		t.Fatalf("flags: %v", flags)
	}
	bySubject := map[string]map[string]any{}
	for _, f := range flags {
		bySubject[f["subject_type"].(string)+"|"+f["subject_id"].(string)] = f
	}
	posting := evZ + ":" + book + ":PRIMARY"
	fj, fp := bySubject["journal|"+jX], bySubject["accounting-posting|"+posting]
	if fj == nil || fp == nil {
		t.Fatalf("expected a flag on journal %s and on posting %s, got %v", jX, posting, bySubject)
	}
	if fj["certification"] != certX || fj["revocation_id"] != "dqx_1" || fj["journal_id"] != jX || fj["status"] != "OPEN" ||
		!strings.Contains(fj["reason"].(string), "amended rows") || fj["legal_entity_id"] != e.entity {
		t.Fatalf("journal flag: %v", fj)
	}
	if fp["certification"] != certZ || fp["revocation_id"] != "rcv_2" || fp["journal_id"] != resZ["journal_id"] {
		t.Fatalf("posting flag: %v", fp)
	}
	// Nothing that does not rest on a revoked certification is flagged, and a kernel journal is flagged once, as its posting.
	for _, f := range flags {
		if f["journal_id"] == jY || f["journal_id"] == jNone {
			t.Fatalf("flagged a journal that does not rest on a revoked certification: %v", f)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM certification_flags WHERE journal_id = $1::uuid`, resZ["journal_id"]); n != 1 {
		t.Fatalf("the kernel journal was flagged %d times", n)
	}

	// Sweeping again finds the same facts: nothing new, and the revoked certification is not asked about again.
	askedBefore := len(e.status.asked)
	again := e.sweep(t, e.entity)
	if again["new_flags"] != float64(0) || again["withdrawn"] != float64(2) || len(e.flags(t, e.entity, "")) != 2 {
		t.Fatalf("second sweep: %v", again)
	}
	if got := len(e.status.asked) - askedBefore; got != 1 { // only certY, still in force
		t.Fatalf("the second sweep asked %d certifications, want only the one still in force", got)
	}
	if e.scalar(t, `SELECT revocation_id FROM certification_watch WHERE run_id = 'dqn_x'`) != "dqx_1" {
		t.Fatal("the withdrawal was not recorded on the watch row")
	}
}

// A journal can record its reliance before a revocation and finalize after the sweep that marked the certification withdrawn.
func TestCertWatch_AJournalPostedAfterTheWithdrawalIsStillCaught(t *testing.T) {
	e := newKEnv(t)
	e.postManual(t, "r-1", certX)
	e.status.set(certX, revokedX)
	if res := e.sweep(t, e.entity); res["new_flags"] != float64(1) {
		t.Fatalf("%v", res)
	}
	late := e.postManual(t, "r-2", certX) // the issuing service is faked as still accepting it: the race
	res := e.sweep(t, e.entity)
	if res["new_flags"] != float64(1) || res["withdrawn"] != float64(1) {
		t.Fatalf("late journal: %v", res)
	}
	found := false
	for _, f := range e.flags(t, e.entity, "OPEN") {
		found = found || f["journal_id"] == late
	}
	if !found {
		t.Fatalf("the journal posted after the withdrawal was not flagged")
	}
	// The first revocation is the one that stays.
	e.status.set(certX, certreliance.Status{State: certreliance.StateRevoked, RevocationID: "dqx_other", Reason: "a different story"})
	e.sweep(t, e.entity)
	if e.scalar(t, `SELECT revocation_id FROM certification_watch WHERE run_id = 'dqn_x'`) != "dqx_1" {
		t.Fatal("a later answer rewrote why the certification was withdrawn")
	}
}

func TestCertWatch_SupersededStaleAndUnreachableAreNotFlagged(t *testing.T) {
	e := newKEnv(t)
	jA := e.postManual(t, "s-1", certX)
	e.postManual(t, "s-2", certY)
	e.postManual(t, "s-3", certZ)
	e.status.set(certX, certreliance.Status{State: certreliance.StateStale})
	e.status.set(certZ, certreliance.Status{State: certreliance.StateSuperseded})
	e.status.errs[certY] = certreliance.ErrUnavailable

	res := e.sweep(t, e.entity)
	if res["new_flags"] != float64(0) || res["withdrawn"] != float64(0) || res["failed"] != float64(1) {
		t.Fatalf("%v", res)
	}
	if len(e.flags(t, e.entity, "")) != 0 {
		t.Fatal("a stale, superseded or unreachable certification produced a flag")
	}
	states := map[string]string{}
	rows, err := e.admin.Query(e.ctx(), `SELECT run_id, COALESCE(last_status, '') FROM certification_watch`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var run, st string
		_ = rows.Scan(&run, &st)
		states[run] = st
	}
	rows.Close()
	// The unreachable one is recorded as nothing: never as in force.
	if states["dqn_x"] != "STALE" || states["rcr_z"] != "SUPERSEDED" || states["dqn_y"] != "" {
		t.Fatalf("recorded states: %v", states)
	}
	_ = jA
}

func TestCertWatch_FlagsAreScopedToTheEntityAndTheTenant(t *testing.T) {
	e := newKEnv(t)
	e.postManual(t, "i-1", certX)
	e.status.set(certX, revokedX)
	e.sweep(t, e.entity)

	other := uuid.NewString()
	if res := e.sweep(t, other); res["checked"] != float64(0) || res["new_flags"] != float64(0) {
		t.Fatalf("sweeping another entity touched this one: %v", res)
	}
	if len(e.flags(t, other, "")) != 0 {
		t.Fatal("another entity sees this entity's flags")
	}
	// Another tenant, same entity id: row-level security shows it nothing.
	st, b := e.call("GET", "/v1/certification-flags?legal_entity_id="+e.entity, uuid.NewString(), "viewer", nil)
	if st != 200 || b["count"] != float64(0) {
		t.Fatalf("another tenant: %d %v", st, b)
	}
}

func TestCertWatch_AcknowledgeRecordsAPersonsDecisionOnce(t *testing.T) {
	e := newKEnv(t)
	jX := e.postManual(t, "a-1", certX)
	jY := e.postManual(t, "a-2", certY)
	fix := e.postManual(t, "a-3", "doc-fix") // the journal that remediates
	e.status.set(certX, revokedX)
	e.status.set(certY, certreliance.Status{State: certreliance.StateRevoked, RevocationID: "dqx_9", Reason: "source defect"})
	e.sweep(t, e.entity)
	flagFor := map[string]string{}
	for _, f := range e.flags(t, e.entity, "OPEN") {
		flagFor[f["journal_id"].(string)] = f["flag_id"].(string)
	}
	ack := func(flag string, body map[string]any) (int, map[string]any) {
		return e.call("POST", "/v1/certification-flags/"+flag+"/acknowledge", e.tenant, "controller", body)
	}

	// Refused: no reason, a reason that is only spaces, an unknown field, an unknown flag, a remediation journal that is not one.
	for name, tc := range map[string]struct {
		flag string
		body map[string]any
		want int
	}{
		"no reason":              {flagFor[jX], map[string]any{}, 400},
		"blank reason":           {flagFor[jX], map[string]any{"acknowledgement": "   "}, 400},
		"unknown field":          {flagFor[jX], map[string]any{"acknowledgement": "ok", "status": "RESOLVED"}, 400},
		"not a journal":          {flagFor[jX], map[string]any{"acknowledgement": "reversed it", "remediation_journal_id": uuid.NewString()}, 400},
		"remediation not a uuid": {flagFor[jX], map[string]any{"acknowledgement": "reversed it", "remediation_journal_id": "x"}, 400},
		"unknown flag":           {uuid.NewString(), map[string]any{"acknowledgement": "ok"}, 404},
		"flag id not a uuid":     {"nope", map[string]any{"acknowledgement": "ok"}, 404},
	} {
		if st, b := ack(tc.flag, tc.body); st != tc.want {
			t.Fatalf("%s: %d %v, want %d", name, st, b, tc.want)
		}
	}
	if len(e.flags(t, e.entity, "OPEN")) != 2 {
		t.Fatal("a refused acknowledgement changed a flag")
	}

	st, got := ack(flagFor[jX], map[string]any{"acknowledgement": "reversed in March; the amount was corrected", "remediation_journal_id": fix})
	if st != 200 || got["status"] != "ACKNOWLEDGED" || got["acknowledged_by"] != "controller" || got["remediation_journal_id"] != fix ||
		got["acknowledged_at"] == nil || got["acknowledgement"] == "" {
		t.Fatalf("%d %v", st, got)
	}
	// Once only: a second acknowledgement, by anyone, is a conflict and rewrites nothing.
	if st, b := ack(flagFor[jX], map[string]any{"acknowledgement": "again"}); st != 409 {
		t.Fatalf("%d %v", st, b)
	}
	if st, _ := ack(flagFor[jY], map[string]any{"acknowledgement": "accepted: immaterial"}); st != 200 {
		t.Fatalf("a flag without a remediation journal: %d", st)
	}
	if len(e.flags(t, e.entity, "OPEN")) != 0 || len(e.flags(t, e.entity, "ACKNOWLEDGED")) != 2 {
		t.Fatal("status filter")
	}
	// Acknowledging reverses nothing: the journals are exactly as they were.
	if e.scalar(t, `SELECT status FROM journal_headers WHERE journal_id = $1::uuid`, jX) != "FINALIZED" {
		t.Fatal("acknowledging a flag changed the journal")
	}
}

func TestCertWatch_EachRouteNeedsItsOwnPermissionAgainstTheRightEntity(t *testing.T) {
	e := newKEnv(t)
	jX := e.postManual(t, "p-1", certX)
	e.status.set(certX, revokedX)
	e.sweep(t, e.entity)
	flag := e.flags(t, e.entity, "")[0]["flag_id"].(string)

	for name, tc := range map[string]struct {
		principal, action, method, path string
		body                            any
	}{
		"sweep":       {"sweeper", "GL_CERTIFICATION_WATCH_SWEEP", "POST", "/v1/certification-watch/sweeps", map[string]any{"legal_entity_id": e.entity}},
		"view":        {"viewer", "GL_CERTIFICATION_FLAG_VIEW", "GET", "/v1/certification-flags?legal_entity_id=" + e.entity, nil},
		"acknowledge": {"controller", "GL_CERTIFICATION_FLAG_ACKNOWLEDGE", "POST", "/v1/certification-flags/" + flag + "/acknowledge", map[string]any{"acknowledgement": "ok"}},
	} {
		e.denied.Store(tc.principal+"|"+tc.action, true)
		if st, _ := e.call(tc.method, tc.path, e.tenant, tc.principal, tc.body); st != 403 {
			t.Fatalf("%s without %s: %d", name, tc.action, st)
		}
		e.denied.Delete(tc.principal + "|" + tc.action)
	}
	// Denial leaves the flag open.
	if e.flags(t, e.entity, "OPEN")[0]["journal_id"] != jX {
		t.Fatal("a denied acknowledgement changed the flag")
	}
	// Malformed requests never reach the store.
	for name, tc := range map[string]struct {
		method, path string
		body         any
	}{
		"sweep without an entity": {"POST", "/v1/certification-watch/sweeps", map[string]any{}},
		"sweep unknown field":     {"POST", "/v1/certification-watch/sweeps", map[string]any{"legal_entity_id": e.entity, "all": true}},
		"flags without an entity": {"GET", "/v1/certification-flags", nil},
		"flags unknown param":     {"GET", "/v1/certification-flags?legal_entity_id=" + e.entity + "&owner=x", nil},
		"flags bad status":        {"GET", "/v1/certification-flags?legal_entity_id=" + e.entity + "&status=DONE", nil},
	} {
		if st, b := e.call(tc.method, tc.path, e.tenant, "viewer", tc.body); st != 400 {
			t.Fatalf("%s: %d %v", name, st, b)
		}
	}
}

// The database holds the rules even if the service is bypassed.
func TestCertWatch_DatabaseHoldsTheRulesToo(t *testing.T) {
	e := newKEnv(t)
	e.postManual(t, "d-1", certX)
	e.postManual(t, "d-0", certZ) // stays in force: a watch row that is not withdrawn
	e.status.set(certX, revokedX)
	e.sweep(t, e.entity)
	flag := e.flags(t, e.entity, "")[0]["flag_id"].(string)
	// One more flag, acknowledged through the service, for the immutability checks.
	e.postManual(t, "d-2", certY)
	e.status.set(certY, certreliance.Status{State: certreliance.StateRevoked, RevocationID: "dqx_9", Reason: "r"})
	e.sweep(t, e.entity)
	var acked string
	for _, f := range e.flags(t, e.entity, "OPEN") {
		if f["flag_id"] != flag {
			acked = f["flag_id"].(string)
		}
	}
	if st, _ := e.call("POST", "/v1/certification-flags/"+acked+"/acknowledge", e.tenant, "controller", map[string]any{"acknowledgement": "ok"}); st != 200 {
		t.Fatal("setup")
	}

	for name, tc := range map[string]struct{ sql, want string }{
		"delete a flag":                {`DELETE FROM certification_flags WHERE flag_id = '` + flag + `'`, "cannot be deleted"},
		"rewrite what was flagged":     {`UPDATE certification_flags SET reason = 'nothing happened' WHERE flag_id = '` + flag + `'`, "cannot be rewritten"},
		"move to another journal":      {`UPDATE certification_flags SET journal_id = gen_random_uuid() WHERE flag_id = '` + flag + `'`, ""},
		"acknowledge without a reason": {`UPDATE certification_flags SET status = 'ACKNOWLEDGED', acknowledged_by = 'x', acknowledged_at = now() WHERE flag_id = '` + flag + `'`, "ack_complete"},
		"acknowledge without a person": {`UPDATE certification_flags SET status = 'ACKNOWLEDGED', acknowledged_at = now(), acknowledgement = 'x' WHERE flag_id = '` + flag + `'`, "ack_complete"},
		"edit an acknowledged flag":    {`UPDATE certification_flags SET acknowledgement = 'rewritten' WHERE flag_id = '` + acked + `'`, "immutable"},
		"reopen an acknowledged flag":  {`UPDATE certification_flags SET status = 'OPEN', acknowledged_by = NULL, acknowledged_at = NULL, acknowledgement = NULL WHERE flag_id = '` + acked + `'`, "immutable"},
		"delete a watch row":           {`DELETE FROM certification_watch`, "cannot be deleted"},
		"un-withdraw a certification":  {`UPDATE certification_watch SET withdrawn_at = NULL, revocation_id = NULL, withdrawal_reason = NULL WHERE run_id = 'dqn_x'`, ""},
		"change a withdrawal reason":   {`UPDATE certification_watch SET withdrawal_reason = 'other' WHERE run_id = 'dqn_x'`, "stays withdrawn"},
		"withdraw without a reason":    {`UPDATE certification_watch SET withdrawn_at = now(), revocation_id = 'x', last_status = 'REVOKED' WHERE run_id = 'rcr_z' AND withdrawn_at IS NULL`, "withdrawn_complete"},
		"flag with no watch row": {`INSERT INTO certification_flags (flag_id, tenant_id, legal_entity_id, certification_service, run_id, subject_type, subject_id, journal_id, revocation_id, reason)
			SELECT gen_random_uuid(), tenant_id, legal_entity_id, 'data-quality-svc', 'dqn_unwatched', 'journal', 'x', journal_id, 'r', 'r' FROM certification_flags LIMIT 1`, "foreign key"},
		"flag the same thing twice": {`INSERT INTO certification_flags (flag_id, tenant_id, legal_entity_id, certification_service, run_id, subject_type, subject_id, journal_id, revocation_id, reason)
			SELECT gen_random_uuid(), tenant_id, legal_entity_id, certification_service, run_id, subject_type, subject_id, journal_id, revocation_id, reason
			FROM certification_flags WHERE flag_id = '` + flag + `'`, "duplicate"},
		"an unknown subject type": {`UPDATE certification_flags SET subject_type = 'invoice' WHERE flag_id = '` + flag + `'`, ""},
	} {
		_, err := e.admin.Exec(e.ctx(), tc.sql)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v, want a refusal mentioning %q", name, err, tc.want)
		}
	}
}

// 000036's down refuses while anything is watched or flagged, and round-trips while nothing is.
func TestMigration000036_DownRefusesWhileTheWatchHoldsAnything(t *testing.T) {
	e := newKEnv(t)
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "../../deployments/migrations")
	down, err := os.ReadFile(filepath.Join(dir, "000036_certification_watch.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	up, err := os.ReadFile(filepath.Join(dir, "000036_certification_watch.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.admin.Exec(e.ctx(), string(down)); err != nil {
		t.Fatalf("down with nothing watched: %v", err)
	}
	if _, err := e.admin.Exec(e.ctx(), string(up)); err != nil {
		t.Fatalf("re-apply up: %v", err)
	}
	if _, err := e.admin.Exec(e.ctx(), `GRANT SELECT, INSERT, UPDATE, DELETE ON certification_watch, certification_flags TO kernel_app`); err != nil {
		t.Fatalf("regrant: %v", err)
	}
	e.postManual(t, "m-1", certX)
	if _, err := e.admin.Exec(e.ctx(), string(down)); err == nil || !strings.Contains(err.Error(), "000036 down refused") {
		t.Fatalf("down with a watched certification: %v", err)
	}
	e.status.set(certX, revokedX)
	e.sweep(t, e.entity)
	if _, err := e.admin.Exec(e.ctx(), string(down)); err == nil || !strings.Contains(err.Error(), "000036 down refused") {
		t.Fatalf("down with a flag: %v", err)
	}
}

// One certification relied on by two legal entities: sweeping and flagging for one entity must not flag the other's journals. This
// is what the entity predicates inside the flagging queries are for; a sweep of an entity with nothing watched cannot show it.
func TestCertWatch_FlaggingNeverCrossesLegalEntities(t *testing.T) {
	e := newKEnv(t)
	mine := e.postManual(t, "x-1", certX)

	other := uuid.NewString()
	body := e.manualBody("x-2", "ADJUSTMENT", "ACCRUAL", 50, certX)
	body["legal_entity_id"] = other
	theirs := e.mustCreate(t, body)
	if st, b := e.call("POST", "/v1/journals/"+theirs+"/validate", e.tenant, "preparer", map[string]any{}); st != 200 {
		t.Fatalf("validate: %d %v", st, b)
	}
	if st, b := e.call("POST", "/v1/journals/"+theirs+"/post", e.tenant, "preparer", map[string]any{}); st != 200 {
		t.Fatalf("post: %d %v", st, b)
	}
	e.status.set(certX, revokedX)

	// Only this entity is swept: only its journal is flagged, though both cite the same revoked certification.
	if res := e.sweep(t, e.entity); res["new_flags"] != float64(1) {
		t.Fatalf("%v", res)
	}
	if got := e.flags(t, e.entity, ""); len(got) != 1 || got[0]["journal_id"] != mine {
		t.Fatalf("this entity's flags: %v", got)
	}
	if got := e.flags(t, other, ""); len(got) != 0 {
		t.Fatalf("the other entity was flagged by a sweep of this one: %v", got)
	}
	// The other entity's own sweep flags its own journal, and it keeps its own watch row.
	if res := e.sweep(t, other); res["new_flags"] != float64(1) {
		t.Fatalf("%v", res)
	}
	if got := e.flags(t, other, ""); len(got) != 1 || got[0]["journal_id"] != theirs || got[0]["legal_entity_id"] != other {
		t.Fatalf("the other entity's flags: %v", got)
	}
	if n := e.count(t, `SELECT count(*) FROM certification_watch WHERE run_id = 'dqn_x'`); n != 2 {
		t.Fatalf("watch rows for the shared certification: %d, want one per entity", n)
	}
}
