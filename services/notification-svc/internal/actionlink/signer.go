package actionlink

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/notification-svc/internal/ledger"
)

var (
	// ErrInvalidSignature indicates the token's cryptographic signature does not match.
	ErrInvalidSignature = errors.New("invalid action token signature")

	// ErrMalformedToken indicates the token string cannot be decoded or parsed.
	ErrMalformedToken = errors.New("malformed action token string")
)

// Signer generates and verifies HMAC-SHA256 signed action tokens per ZS-COMMS-EMAIL-001 §6.
type Signer struct {
	secretKey []byte
	baseURL   string
}

// NewSigner creates a new action link signer with the provided HMAC secret key and base gateway URL.
func NewSigner(secretKey []byte, baseURL string) (*Signer, error) {
	if len(secretKey) < 16 {
		return nil, errors.New("action link signer secret key must be at least 16 bytes")
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &Signer{
		secretKey: secretKey,
		baseURL:   baseURL,
	}, nil
}

// GenerateToken generates a cryptographically secure random token, computes its SHA-256 database
// hash, computes an HMAC signature, and returns the persisted ActionToken struct along with the signed URL string.
func (s *Signer) GenerateToken(
	tenantID, messageIntentID, recipientPrincipalID, purpose, targetURL, targetMethod string,
	payload []byte, ttl time.Duration,
) (*ledger.ActionToken, string, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, "", errors.New("missing tenant_id")
	}
	if strings.TrimSpace(messageIntentID) == "" {
		return nil, "", errors.New("missing message_intent_id")
	}
	if strings.TrimSpace(recipientPrincipalID) == "" {
		return nil, "", errors.New("missing recipient_principal_id")
	}
	if strings.TrimSpace(purpose) == "" {
		return nil, "", errors.New("missing purpose")
	}
	if strings.TrimSpace(targetURL) == "" {
		return nil, "", errors.New("missing target_url")
	}
	if targetMethod == "" {
		targetMethod = "POST"
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}

	// 1. Generate 32 bytes of cryptographic randomness
	rawBytes := make([]byte, 32)
	if _, err := rand.Read(rawBytes); err != nil {
		return nil, "", fmt.Errorf("generate random token bytes: %w", err)
	}
	rawSecret := hex.EncodeToString(rawBytes)

	// 2. Compute SHA-256 hash for database storage and indexing
	tokenHash := s.ComputeHash(rawSecret)

	// 3. Compute HMAC signature over (tenant_id : purpose : rawSecret)
	signature := s.computeHMAC(tenantID, purpose, rawSecret)

	// 4. Encode signed token string: base64url(tenant_id . purpose . rawSecret . signature)
	tokenPayload := fmt.Sprintf("%s.%s.%s.%s", tenantID, purpose, rawSecret, signature)
	signedTokenStr := base64.RawURLEncoding.EncodeToString([]byte(tokenPayload))

	now := time.Now().UTC()
	actionToken := &ledger.ActionToken{
		TokenID:              uuid.NewString(),
		TokenHash:            tokenHash,
		MessageIntentID:      messageIntentID,
		TenantID:             tenantID,
		RecipientPrincipalID: recipientPrincipalID,
		Purpose:              purpose,
		TargetActionURL:      targetURL,
		TargetMethod:         targetMethod,
		Payload:              payload,
		Status:               ledger.ActionTokenStatusActive,
		ExpiresAt:            now.Add(ttl),
		CreatedAt:            now,
	}

	return actionToken, signedTokenStr, nil
}

// VerifyAndExtract verifies the HMAC signature of the signed token string and returns the tenant ID,
// the purpose, the raw secret, and the SHA-256 database token hash.
func (s *Signer) VerifyAndExtract(signedTokenStr string) (tenantID string, purpose string, rawSecret string, tokenHash string, err error) {
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(signedTokenStr))
	if err != nil {
		return "", "", "", "", fmt.Errorf("%w: base64 decode failed", ErrMalformedToken)
	}

	parts := strings.Split(string(decoded), ".")
	if len(parts) != 4 {
		return "", "", "", "", fmt.Errorf("%w: invalid token segments count (%d)", ErrMalformedToken, len(parts))
	}

	tenantID = parts[0]
	purpose = parts[1]
	rawSecret = parts[2]
	givenSig := parts[3]

	expectedSig := s.computeHMAC(tenantID, purpose, rawSecret)
	if !hmac.Equal([]byte(givenSig), []byte(expectedSig)) {
		return "", "", "", "", ErrInvalidSignature
	}

	tokenHash = s.ComputeHash(rawSecret)
	return tenantID, purpose, rawSecret, tokenHash, nil
}

// ComputeHash computes the SHA-256 hex digest of a raw token string.
func (s *Signer) ComputeHash(rawSecret string) string {
	h := sha256.Sum256([]byte(rawSecret))
	return hex.EncodeToString(h[:])
}

// GenerateURL constructs the full canonical action link URL from a signed token string.
func (s *Signer) GenerateURL(signedTokenStr string) string {
	if s.baseURL == "" {
		return fmt.Sprintf("/v1/notifications/actions/%s", signedTokenStr)
	}
	return fmt.Sprintf("%s/v1/notifications/actions/%s", s.baseURL, signedTokenStr)
}

func (s *Signer) computeHMAC(tenantID, purpose, rawSecret string) string {
	mac := hmac.New(sha256.New, s.secretKey)
	mac.Write([]byte(tenantID + ":" + purpose + ":" + rawSecret))
	return hex.EncodeToString(mac.Sum(nil))
}
