package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

func TestNormalizeEmail(t *testing.T) {
	for in, want := range map[string]string{
		"  Ap@Example.COM ":       "Ap@example.com",
		"billing@sub.Example.org": "billing@sub.example.org",
		"first.last+tag@x.io":     "first.last+tag@x.io",
	} {
		got, err := domain.NormalizeEmail(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got)
	}
	long := strings.Repeat("a", 250) + "@b.co"
	for _, bad := range []string{
		"", "   ", "plain", "a@b", "@example.com", "a@", "a@.com", "a@example.", "a b@example.com",
		"Ann <ann@example.com>", "<ann@example.com>", "a@example.com, b@example.com", "a@example.com;b@example.com",
		"a@exam\nple.com", "a@exa mple.com", long,
	} {
		_, err := domain.NormalizeEmail(bad)
		assert.Error(t, err, "%q must be refused", bad)
	}
	_, err := domain.NormalizeEmail(strings.Repeat("a", 242) + "@example.com") // exactly 254
	assert.NoError(t, err)
	_, err = domain.NormalizeEmail(strings.Repeat("a", 243) + "@example.com") // 255
	assert.Error(t, err)
}

func TestContactDigestIsStableAndDoesNotContainTheValue(t *testing.T) {
	d := domain.ContactDigest("a@example.com")
	assert.Len(t, d, 64)
	assert.Equal(t, d, domain.ContactDigest("a@example.com"))
	assert.NotEqual(t, d, domain.ContactDigest("b@example.com"))
	assert.NotContains(t, d, "example")
}

func TestContactStateMachine(t *testing.T) {
	assert.True(t, domain.ContactProposed.CanMoveTo(domain.ContactVerified))
	assert.True(t, domain.ContactProposed.CanMoveTo(domain.ContactRetired))
	assert.True(t, domain.ContactVerified.CanMoveTo(domain.ContactRetired))
	assert.False(t, domain.ContactVerified.CanMoveTo(domain.ContactVerified))
	assert.False(t, domain.ContactVerified.CanMoveTo(domain.ContactProposed))
	for _, to := range []domain.ContactStatus{domain.ContactProposed, domain.ContactVerified, domain.ContactRetired} {
		assert.False(t, domain.ContactRetired.CanMoveTo(to), "RETIRED is final")
	}
	assert.False(t, domain.ContactStatus("X").Valid())
}

func TestCreateContactPointRequestNormalizedDefaultsAndValidates(t *testing.T) {
	ok := domain.CreateContactPointRequest{Channel: domain.ContactEmail, Value: " A@EXAMPLE.com", SourceClass: domain.SourceTenantAsserted, EvidenceRef: " e "}
	n, err := ok.Normalized()
	require.NoError(t, err)
	assert.Equal(t, "A@example.com", n.Value)
	assert.Equal(t, domain.ContactGeneral, n.Purpose)
	assert.Equal(t, "e", n.EvidenceRef)

	for name, mut := range map[string]func(r *domain.CreateContactPointRequest){
		"channel":   func(r *domain.CreateContactPointRequest) { r.Channel = "SMS" },
		"purpose":   func(r *domain.CreateContactPointRequest) { r.Purpose = "MARKETING" },
		"source":    func(r *domain.CreateContactPointRequest) { r.SourceClass = "" },
		"value":     func(r *domain.CreateContactPointRequest) { r.Value = "nope" },
		"nochannel": func(r *domain.CreateContactPointRequest) { r.Channel = "" },
	} {
		r := ok
		mut(&r)
		_, err := r.Normalized()
		assert.Error(t, err, name)
	}
}

func TestVerifyAndRetireRequestsNeedTheirJustification(t *testing.T) {
	assert.Error(t, domain.VerifyContactPointRequest{}.Validate())
	assert.Error(t, domain.VerifyContactPointRequest{EvidenceRef: "  "}.Validate())
	assert.NoError(t, domain.VerifyContactPointRequest{EvidenceRef: "mail-roundtrip"}.Validate())
	assert.Error(t, domain.RetireContactPointRequest{}.Validate())
	assert.NoError(t, domain.RetireContactPointRequest{Reason: "bounced"}.Validate())
}

func TestContactPointInForceIsHalfOpen(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	c := domain.ContactPoint{ValidFrom: from, ValidTo: &to}
	assert.False(t, c.InForce(from.Add(-time.Nanosecond)))
	assert.True(t, c.InForce(from), "valid_from is inclusive")
	assert.True(t, c.InForce(to.Add(-time.Nanosecond)))
	assert.False(t, c.InForce(to), "valid_to is exclusive")
	assert.True(t, domain.ContactPoint{ValidFrom: from}.InForce(to.Add(1000*time.Hour)), "no end means in force")
}

func TestContactPointQueryValidationAndValueRedaction(t *testing.T) {
	assert.NoError(t, domain.ContactPointQuery{}.Validate())
	assert.NoError(t, domain.ContactPointQuery{Channel: domain.ContactEmail, Status: domain.ContactVerified}.Validate())
	assert.Error(t, domain.ContactPointQuery{Channel: "SMS"}.Validate())
	assert.Error(t, domain.ContactPointQuery{Status: "GONE"}.Validate())
	c := domain.ContactPoint{Value: "a@example.com"}
	assert.Empty(t, c.WithoutValue().Value)
	assert.Equal(t, "a@example.com", c.Item().Value)
}
