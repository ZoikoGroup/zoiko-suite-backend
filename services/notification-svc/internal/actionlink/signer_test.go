package actionlink_test

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/notification-svc/internal/actionlink"
	"zoiko.io/notification-svc/internal/ledger"
)

var testKey = []byte("super-secure-hmac-secret-key-32b")

func TestSigner_GenerateAndVerify(t *testing.T) {
	signer, err := actionlink.NewSigner(testKey, "https://notify.zoiko.com")
	require.NoError(t, err)

	tenantID := "tenant-alpha"
	intentID := "intent-123"
	recipientID := "usr-789"
	purpose := "VERIFY_EMAIL"
	targetURL := "https://auth.zoiko.com/complete-verification"

	token, signedTokenStr, err := signer.GenerateToken(
		tenantID, intentID, recipientID, purpose, targetURL, "POST",
		[]byte(`{"email":"alice@example.com"}`), 1*time.Hour,
	)
	require.NoError(t, err)
	assert.NotEmpty(t, token.TokenID)
	assert.NotEmpty(t, token.TokenHash)
	assert.Equal(t, ledger.ActionTokenStatusActive, token.Status)
	assert.NotEmpty(t, signedTokenStr)

	// URL generation
	url := signer.GenerateURL(signedTokenStr)
	assert.True(t, strings.HasPrefix(url, "https://notify.zoiko.com/v1/notifications/actions/"))

	// Verify and extract
	gotTenant, gotPurpose, gotSecret, gotHash, err := signer.VerifyAndExtract(signedTokenStr)
	require.NoError(t, err)
	assert.Equal(t, tenantID, gotTenant)
	assert.Equal(t, purpose, gotPurpose)
	assert.NotEmpty(t, gotSecret)
	assert.Equal(t, token.TokenHash, gotHash)
}

func TestSigner_TamperedToken_FailsVerification(t *testing.T) {
	signer, err := actionlink.NewSigner(testKey, "")
	require.NoError(t, err)

	_, signedTokenStr, err := signer.GenerateToken(
		"tenant-1", "intent-1", "user-1", "RESET_PASSWORD", "https://example.com", "POST",
		nil, 1*time.Hour,
	)
	require.NoError(t, err)

	decodedBytes, err := base64.RawURLEncoding.DecodeString(signedTokenStr)
	require.NoError(t, err)

	parts := strings.Split(string(decodedBytes), ".")
	require.Len(t, parts, 4)

	// 1. Tamper tenant
	tamperedTenant := "tenant-hacked." + parts[1] + "." + parts[2] + "." + parts[3]
	tamperedStr := base64.RawURLEncoding.EncodeToString([]byte(tamperedTenant))
	_, _, _, _, err = signer.VerifyAndExtract(tamperedStr)
	assert.ErrorIs(t, err, actionlink.ErrInvalidSignature)

	// 2. Tamper purpose
	tamperedPurpose := parts[0] + ".ADMIN_ELEVATE." + parts[2] + "." + parts[3]
	tamperedStr = base64.RawURLEncoding.EncodeToString([]byte(tamperedPurpose))
	_, _, _, _, err = signer.VerifyAndExtract(tamperedStr)
	assert.ErrorIs(t, err, actionlink.ErrInvalidSignature)

	// 3. Tamper secret
	tamperedSecret := parts[0] + "." + parts[1] + ".tamperedsecret." + parts[3]
	tamperedStr = base64.RawURLEncoding.EncodeToString([]byte(tamperedSecret))
	_, _, _, _, err = signer.VerifyAndExtract(tamperedStr)
	assert.ErrorIs(t, err, actionlink.ErrInvalidSignature)
}

func TestSigner_MalformedTokens(t *testing.T) {
	signer, err := actionlink.NewSigner(testKey, "")
	require.NoError(t, err)

	// Invalid base64
	_, _, _, _, err = signer.VerifyAndExtract("!@#$%^&*")
	assert.ErrorIs(t, err, actionlink.ErrMalformedToken)

	// Wrong segments
	tooFew := base64.RawURLEncoding.EncodeToString([]byte("a.b.c"))
	_, _, _, _, err = signer.VerifyAndExtract(tooFew)
	assert.ErrorIs(t, err, actionlink.ErrMalformedToken)
}

func TestSigner_ShortSecretKey_Rejected(t *testing.T) {
	_, err := actionlink.NewSigner([]byte("too-short"), "")
	assert.Error(t, err, "signer should reject keys under 16 bytes")
}
