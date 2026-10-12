package context_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/siem"
)

// S1-B1: web vs mobile for a human is settled by the VERIFIED IdP client
// (azp), not by the client's header, and travels on the signed envelope for
// gateway-auth-svc to stamp.
func resolveWithClient(t *testing.T, azp, asserted string) (domain.SessionContext, *domain.IdentityContextEnvelope) {
	t.Helper()
	f := defaultFixture()
	claims := *validClaims
	claims.ClientID = azp
	f.verifier = &mockTokenVerifier{claims: &claims}
	cfg := *testCfg
	cfg.IDPClientChannels = map[string]string{"zoiko-console": "web", "zoiko-mobile": "mobile"}
	r := identityctx.NewResolver(&cfg, zap.NewNop(), f.principals, f.sessions, f.riskSignals, f.upstream, f.events,
		f.verifier, f.signer, siem.New("", "identity-context-svc", zap.NewNop()))
	req := baseRequest
	req.SourceChannel = asserted
	_, err := r.Resolve(context.Background(), req)
	require.NoError(t, err)
	require.Len(t, f.sessions.storedCtx, 1)
	for _, sc := range f.sessions.storedCtx {
		return *sc, f.signer.captured
	}
	return domain.SessionContext{}, nil
}

func TestSourceChannel_VerifiedClientSettlesWebVsMobile(t *testing.T) {
	sc, env := resolveWithClient(t, "zoiko-mobile", "web") // the header lies
	assert.Equal(t, "mobile", sc.SourceChannel)
	assert.Equal(t, domain.BasisVerifiedClient, sc.SourceChannelBasis)
	assert.Equal(t, "mobile", env.SourceChannel, "the verified channel travels on the envelope")

	sc, env = resolveWithClient(t, "zoiko-console", "")
	assert.Equal(t, "web", sc.SourceChannel)
	assert.Equal(t, "web", env.SourceChannel)
}

// An unmapped client falls back to the checked assertion, which is NOT put on
// the envelope as fact.
func TestSourceChannel_UnmappedClientStaysAnAssertion(t *testing.T) {
	sc, env := resolveWithClient(t, "some-other-client", "api")
	assert.Equal(t, "api", sc.SourceChannel)
	assert.Equal(t, domain.BasisAssertedConsistent, sc.SourceChannelBasis)
	assert.Empty(t, env.SourceChannel)
}
