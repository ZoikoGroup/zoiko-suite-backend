package ar09

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// Receivables is the READ-ONLY view of AR-07 that collections is allowed. It deliberately has no adjustment, creation or
// close method: AR-09 cannot create, settle, write off or reverse a balance (invariant 10). *ar07.Module satisfies it.
type Receivables interface {
	GetByID(ctx context.Context, tenantID, openItemID string) (*kit.OpenItemView, error)
	CollectionBlocked(ctx context.Context, tenantID, legalEntityID, customerPartyID string) (bool, error)
	ListOpenForCustomer(ctx context.Context, tenantID, legalEntityID, customerPartyID, currency string) ([]kit.OpenItemView, error)
}

// Module is AR-09.
type Module struct {
	deps      kit.Deps
	recv      Receivables
	profiles  kit.ProfileResolver // AR-01 (*ar01.ProfileResolver)
	deliverer Deliverer
}

// New builds the module. deliverer nil = UnconfiguredDeliverer (every notice is recorded DELIVERY_FAILED).
func New(deps kit.Deps, recv Receivables, profiles kit.ProfileResolver, deliverer Deliverer) *Module {
	if deliverer == nil {
		deliverer = UnconfiguredDeliverer{}
	}
	return &Module{deps: deps, recv: recv, profiles: profiles, deliverer: deliverer}
}

// Register mounts the spec paths verbatim at the router root.
func (m *Module) Register(r chi.Router) {
	for _, q := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"getCollectionCase", m.getCollectionCase},
		{"getDunningSchedule", m.getDunningSchedule},
		{"getCustomerCollectionHistory", m.getCustomerCollectionHistory},
		{"getAvailableActions", m.getAvailableActions},
		{"getPromisesDue", m.getPromisesDue},
	} {
		h := m.deps.Query(q.h)
		r.Get("/internal/v1/ar09/"+q.name, h)
		r.Post("/internal/v1/ar09/"+q.name, h)
	}
	for _, c := range []struct {
		path, op string
		h        http.HandlerFunc
	}{
		{"openCollectionCase", "AR09.OpenCollectionCase", m.openCollectionCase},
		{"advanceDunningStage", "AR09.AdvanceDunningStage", m.advanceDunningStage},
		{"sendDunningNotice", "AR09.SendDunningNotice", m.sendDunningNotice},
		{"recordPromiseToPay", "AR09.RecordPromiseToPay", m.recordPromiseToPay},
		{"placeCollectionHold", "AR09.PlaceCollectionHold", m.placeCollectionHold},
		{"resumeCollections", "AR09.ResumeCollections", m.resumeCollections},
		{"escalateCollectionCase", "AR09.EscalateCollectionCase", m.escalateCollectionCase},
		{"closeCollectionCase", "AR09.CloseCollectionCase", m.closeCollectionCase},
		{"publishDunningPolicy", "AR09.PublishDunningPolicy", m.publishDunningPolicy}, // policy data; not a spec command
	} {
		r.Post("/internal/v1/ar09/commands/"+c.path, m.deps.Cmd(c.op, c.h))
	}
}

func (m *Module) read(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	return m.deps.DB.WithTx(ctx, tenantID, fn)
}

func (m *Module) fail(w http.ResponseWriter, err error) {
	err = kit.MapPgErr(err)
	var p *kit.Problem
	if !errors.As(err, &p) {
		m.deps.Log.Error("ar09 request failed", zap.Error(err))
	}
	kit.WriteErr(w, err)
}

// checkAction is Authorize without writing the response.
func (m *Module) checkAction(ctx context.Context, c kit.Caller, action string) error {
	err := m.deps.Authz.CheckAllowed(ctx, c.PrincipalID, c.LegalEntityID, action)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrAuthorizationDenied):
		return kit.Forbidden(kit.CodeForbidden, "")
	default:
		m.deps.Log.Error("authorization check failed - failing closed", zap.Error(err))
		return kit.Unavailable("authorization service unavailable")
	}
}

// ---- scheduler ----

// RunDue advances every live case that is due under its pinned policy as of asOf: it syncs dispute / promise-lapse state
// from AR-07 facts, resolves cases whose items AR-07 shows settled, advances the stage and sends the stage notice. It is
// idempotent per (case, stage): a replay or a concurrent run produces one notice per stage (row lock + SKIP LOCKED + the
// unique SENT/DELIVERED index), is suppression-aware (dispute / hold / profile / promise grace / blackout / rate limit are
// re-checked inside each case transaction) and never blocks on a case another worker is processing.
//
// processed counts cases whose state moved or whose notice was attempted. A failure on one case is logged and joined into
// the returned error; the other cases still run.
//
// The cross-tenant candidate scan needs a database role that is not subject to the tenant RLS policy (like the outbox
// dispatcher); under a restricted role it simply finds nothing.
func (m *Module) RunDue(ctx context.Context, asOf time.Time) (int, error) {
	return m.runDue(ctx, asOf, "")
}

// runDue is RunDue limited to one tenant when tenantID is set (tests; a future per-tenant scheduler).
func (m *Module) runDue(ctx context.Context, asOf time.Time, tenantID string) (int, error) {
	type ref struct{ tenant, id string }
	var refs []ref
	rows, err := m.deps.DB.Pool.Query(ctx, `SELECT tenant_id::text, case_id::text FROM ar09_collection_cases
		WHERE state NOT IN ('RESOLVED','CLOSED','ESCALATED','HELD') AND ($1 = '' OR tenant_id::text = $1) ORDER BY created_at, case_id LIMIT 5000`, tenantID)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.tenant, &r.id); err != nil {
			rows.Close()
			return 0, err
		}
		refs = append(refs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	processed := 0
	var errs []error
	for _, r := range refs {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		did, err := m.runCase(ctx, r.tenant, r.id, asOf.UTC())
		if err != nil {
			m.deps.Log.Warn("ar09 RunDue case failed", zap.String("case_id", r.id), zap.Error(err))
			errs = append(errs, err)
			continue
		}
		if did {
			processed++
		}
	}
	return processed, errors.Join(errs...)
}

func (m *Module) runCase(ctx context.Context, tenantID, caseID string, now time.Time) (bool, error) {
	did := false
	err := m.deps.DB.WithTx(ctx, tenantID, func(tx pgx.Tx) error {
		cs, err := getCase(ctx, tx, tenantID, caseID, true, true)
		var p *kit.Problem
		if errors.As(err, &p) && p.Code == kit.CodeNotFound {
			return nil // locked by another worker (or gone): they own it
		}
		if err != nil {
			return err
		}
		if IsTerminal(cs.State) || cs.State == StateEscalated || cs.State == StateHeld {
			return nil
		}
		c := kit.Caller{TenantID: tenantID, PrincipalID: SystemPrincipal, CorrelationID: "ar09-run-" + uuid.NewString(), LegalEntityID: cs.LegalEntityID}
		pol, err := policyByID(ctx, tx, tenantID, cs.PolicyID)
		if err != nil {
			return err
		}
		lv, err := m.live(ctx, tx, cs, now)
		if err != nil {
			return err
		}
		if ok, err := m.resolveIfSettled(ctx, tx, c, cs, lv, now, "AUTO_RESOLVED"); err != nil || ok {
			did = did || ok
			return err
		}
		// Derived state sync: a dispute / lapsed promise is read from facts, never trusted from a stale status.
		if moved, err := m.syncState(ctx, tx, c, cs, pol, lv, now); err != nil {
			return err
		} else if moved {
			did = true
		}
		res, err := m.doAdvance(ctx, tx, c, cs, pol, lv, now, true)
		if err != nil {
			var pr *kit.Problem
			if !(errors.As(err, &pr) && pr.Code == kit.CodeInvalidTransition) {
				return err
			}
		}
		if res.Done {
			did = true
		}
		if res.Suppressed != "" {
			return nil
		}
		if cs.DunningStage >= 1 {
			sres, err := m.doSend(ctx, tx, c, cs, pol, lv, now, true)
			if err != nil {
				var pr *kit.Problem
				if !(errors.As(err, &pr) && pr.Code == kit.CodeInvalidTransition) {
					return err
				}
			}
			if sres.Done {
				did = true
			}
		}
		return nil
	})
	return did, kit.MapPgErr(err)
}

// syncState keeps the derived case states consistent with AR-07 facts: ACTIVE/PROMISE_TO_PAY/ELIGIBLE -> DISPUTED while an
// item is disputed, DISPUTED -> prior state once none is, PROMISE_TO_PAY -> ACTIVE when the promise and its grace elapsed.
func (m *Module) syncState(ctx context.Context, tx pgx.Tx, c kit.Caller, cs *Case, pol *Policy, lv *liveView, now time.Time) (bool, error) {
	disputed := false
	for _, v := range lv.views {
		if v.Residual.IsPositive() && v.Disputed {
			disputed = true
		}
	}
	from := cs.State
	switch {
	case disputed && (cs.State == StateEligible || cs.State == StateActive || cs.State == StatePromiseToPay):
		cs.StateBefore, cs.State = cs.State, StateDisputed
	case !disputed && cs.State == StateDisputed:
		cs.State, cs.StateBefore = cs.StateBefore, ""
		if cs.State == "" || cs.State == StateDisputed {
			cs.State = StateActive
		}
	case cs.State == StatePromiseToPay:
		if r, err := m.contactReason(ctx, tx, cs, &Policy{PromiseGraceDays: pol.PromiseGraceDays}, now); err != nil {
			return false, err
		} else if r == SuppPromiseGrace {
			return false, nil
		}
		cs.State = StateActive
	default:
		return false, nil
	}
	if err := saveCase(ctx, tx, cs, now); err != nil {
		return false, err
	}
	return true, addHistory(ctx, tx, c, cs, "STATE_SYNCED", from, cs.State, "", map[string]any{"source": "AR-07 live read"})
}

// Start runs RunDue every interval until ctx ends. The caller starts it only behind an explicit flag.
func (m *Module) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := m.RunDue(ctx, m.deps.Now()); err != nil {
				m.deps.Log.Warn("ar09 scheduler run finished with errors", zap.Int("processed", n), zap.Error(err))
			}
		}
	}
}
