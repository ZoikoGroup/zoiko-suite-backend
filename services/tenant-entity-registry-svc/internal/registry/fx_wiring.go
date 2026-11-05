package registry

import "zoiko.io/tenant-entity-registry-svc/internal/currency"

// WithCurrencyValidator sets the read-only currency-registry check the exchange-rate commands use. Without one they
// fail closed: a rate is never accepted for a currency nobody confirmed exists.
func (s *Service) WithCurrencyValidator(v currency.Validator) *Service {
	s.currencies = v
	return s
}
