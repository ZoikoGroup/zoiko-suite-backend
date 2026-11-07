package registry

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/currency"
	"zoiko.io/tenant-entity-registry-svc/internal/fiscalcal"
)

// WithCurrencyValidator sets the read-only currency-registry check the exchange-rate commands use. Without one they
// fail closed: a rate is never accepted for a currency nobody confirmed exists.
func (s *Service) WithCurrencyValidator(v currency.Validator) *Service {
	s.currencies = v
	return s
}

// WithFiscalCalendarValidator sets the read-only fiscal-calendar-svc check (ORG-03 s4.3). Without one the fiscal
// calendar reference is format-checked only, as before REF-04 existed.
func (s *Service) WithFiscalCalendarValidator(v fiscalcal.Validator) *Service {
	s.fiscal = v
	return s
}

// checkFiscalCalendar confirms the entity's fiscal calendar exists for its tenant. An unknown calendar is a bad
// reference; an unreachable service fails closed.
func (s *Service) checkFiscalCalendar(ctx context.Context, tenantID, calendarID string) error {
	if s.fiscal == nil {
		return nil
	}
	calendarID = strings.TrimSpace(calendarID)
	ok, err := s.fiscal.Exists(ctx, tenantID, calendarID)
	if err != nil {
		s.log.Error("fiscal calendar service unavailable: rejecting entity (fail-closed)", zap.String("fiscal_calendar_id", calendarID), zap.Error(err))
		return ErrServiceUnavailable
	}
	if !ok {
		return fmt.Errorf("%w: fiscal_calendar_id %s is not a fiscal calendar of this tenant", ErrReferenceInvalid, calendarID)
	}
	return nil
}
