package registry

import "fmt"

// ErrVersionRequired — a named command arrived without expected_version.
// 400, VALIDATION_FAILED.
//
// ORG §4.2 "lifecycle commands use expected_version", §4.3 "commands use UUID
// and expected_version", §3 "All material master updates require optimistic
// concurrency". Until 28 Sep 2026 a missing version was replaced with the one
// the service had just read, which guards against a race inside one request
// but not against the case the control exists for: a caller acting on a
// view it read minutes ago. That is now refused; the old substitution is
// kept only behind the dev-only EXPECTED_VERSION_OPTIONAL flag.
var ErrVersionRequired error = &kindError{"expected_version is required: send the record_version you last read", ErrInvalidInput}

// ConfigureConcurrency sets the dev-only compatibility switch.
func (s *Service) ConfigureConcurrency(expectedVersionOptional bool) {
	s.expectedVersionOptional = expectedVersionOptional
}

// checkExpected is the one place a command's expected_version is decided:
// required (unless the dev flag), and equal to the record's current version.
func (s *Service) checkExpected(supplied, current int64, what string) (int64, error) {
	if supplied == 0 {
		if !s.expectedVersionOptional {
			return 0, ErrVersionRequired
		}
		return current, nil
	}
	if supplied != current {
		return 0, fmt.Errorf("%w: you supplied expected_version %d but this %s is at %d — reload and retry",
			ErrVersionConflict, supplied, what, current)
	}
	return supplied, nil
}
