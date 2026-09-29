package vault

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// EnvelopeFileVaultBackend is the KMS-backed Backend (SEC-INV-07). Material
// is encrypted with a fresh AES-256-GCM data key per write; the data key is
// stored only in wrapped form, wrapped by a KeyWrapper whose master key never
// leaves the KMS. The lease-token signing key is likewise its own random key,
// stored wrapped — so, unlike LocalFileVaultBackend, the key that signs
// tokens is not the key that decrypts the store.
//
// Crypto versioning (SEC-INV-22): every record names its algorithm and the
// KEK that wraps it, so a future algorithm or a KMS key change is a new
// record version rather than a silent reinterpretation of old bytes.
type EnvelopeFileVaultBackend struct {
	mu         sync.Mutex
	filePath   string
	kek        KeyWrapper
	signingKey []byte
}

// AlgEnvelopeAES256GCMv1 is the only algorithm this backend writes.
const AlgEnvelopeAES256GCMv1 = "envelope/aes-256-gcm/v1"

const envelopeStoreFormat = "zoiko-secret-store/envelope/v1"

// ErrLegacyStoreFormat means the store file was written by
// LocalFileVaultBackend. It is refused rather than reinterpreted; migrate it
// with MigrateLocalStore.
var ErrLegacyStoreFormat = errors.New("vault: store file is in the legacy local-key format; migrate it (VAULT_MIGRATE_LEGACY_KEY_FILE)")

type envelopeRecord struct {
	Alg           string `json:"alg"`
	KeyRef        string `json:"key_ref"`
	WrappedDEK    string `json:"wrapped_dek"`
	NonceB64      string `json:"nonce_b64"`
	CiphertextB64 string `json:"ciphertext_b64"`
}

type wrappedKey struct {
	KeyRef  string `json:"key_ref"`
	Wrapped string `json:"wrapped"`
}

type envelopeStore struct {
	Format     string                    `json:"format"`
	SigningKey wrappedKey                `json:"lease_signing_key"`
	Records    map[string]envelopeRecord `json:"records"`
}

// NewEnvelopeFileVaultBackend opens (or initialises) the envelope store at
// filePath. The lease-signing key is unwrapped once here; a KMS that cannot
// be reached at startup fails the boot, which is the fail-closed posture.
func NewEnvelopeFileVaultBackend(ctx context.Context, filePath string, kek KeyWrapper) (*EnvelopeFileVaultBackend, error) {
	if kek == nil {
		return nil, errors.New("vault: envelope backend needs a KEK provider")
	}
	b := &EnvelopeFileVaultBackend{filePath: filePath, kek: kek}
	st, err := b.load()
	if err != nil {
		return nil, err
	}
	if st.SigningKey.Wrapped == "" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("vault: generate lease signing key: %w", err)
		}
		wrapped, err := kek.Wrap(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("vault: wrap lease signing key: %w", err)
		}
		st.SigningKey = wrappedKey{KeyRef: kek.KeyRef(), Wrapped: wrapped}
		if err := b.save(st); err != nil {
			return nil, err
		}
		b.signingKey = key
		return b, nil
	}
	key, err := kek.Unwrap(ctx, st.SigningKey.Wrapped)
	if err != nil {
		return nil, fmt.Errorf("vault: unwrap lease signing key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("vault: lease signing key unwrapped to %d bytes, want 32", len(key))
	}
	b.signingKey = key
	return b, nil
}

func (b *EnvelopeFileVaultBackend) load() (envelopeStore, error) {
	empty := envelopeStore{Format: envelopeStoreFormat, Records: map[string]envelopeRecord{}}
	data, err := os.ReadFile(b.filePath)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(data) == 0) {
		return empty, nil
	}
	if err != nil {
		return envelopeStore{}, fmt.Errorf("vault: failed to read store: %w", err)
	}
	var st envelopeStore
	if err := json.Unmarshal(data, &st); err != nil {
		// A legacy file whose secret paths include "format" or "records"
		// fails to decode as this shape; recognise it as legacy, not corrupt.
		var legacy map[string]record
		if json.Unmarshal(data, &legacy) == nil {
			return envelopeStore{}, ErrLegacyStoreFormat
		}
		return envelopeStore{}, fmt.Errorf("vault: store is corrupt: %w", err)
	}
	if st.Format != envelopeStoreFormat {
		return envelopeStore{}, ErrLegacyStoreFormat
	}
	if st.Records == nil {
		st.Records = map[string]envelopeRecord{}
	}
	return st, nil
}

func (b *EnvelopeFileVaultBackend) save(st envelopeStore) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("vault: failed to marshal store: %w", err)
	}
	tmp := b.filePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("vault: failed to write store: %w", err)
	}
	return os.Rename(tmp, b.filePath)
}

func (b *EnvelopeFileVaultBackend) seal(ctx context.Context, secretPath string, plaintext []byte) (envelopeRecord, error) {
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return envelopeRecord{}, fmt.Errorf("vault: generate data key: %w", err)
	}
	defer clear(dek)
	gcm, err := newGCM(dek)
	if err != nil {
		return envelopeRecord{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return envelopeRecord{}, fmt.Errorf("vault: generate nonce: %w", err)
	}
	wrapped, err := b.kek.Wrap(ctx, dek)
	if err != nil {
		return envelopeRecord{}, err
	}
	// The secret path is the AAD: a record copied under another path fails
	// to open instead of serving one secret's material as another's.
	ct := gcm.Seal(nil, nonce, plaintext, []byte(secretPath))
	return envelopeRecord{
		Alg:           AlgEnvelopeAES256GCMv1,
		KeyRef:        b.kek.KeyRef(),
		WrappedDEK:    wrapped,
		NonceB64:      base64.StdEncoding.EncodeToString(nonce),
		CiphertextB64: base64.StdEncoding.EncodeToString(ct),
	}, nil
}

func (b *EnvelopeFileVaultBackend) open(ctx context.Context, secretPath string, rec envelopeRecord) ([]byte, error) {
	if rec.Alg != AlgEnvelopeAES256GCMv1 {
		return nil, fmt.Errorf("vault: unsupported record algorithm %q", rec.Alg)
	}
	dek, err := b.kek.Unwrap(ctx, rec.WrappedDEK)
	if err != nil {
		return nil, err
	}
	defer clear(dek)
	gcm, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	nonce, err := base64.StdEncoding.DecodeString(rec.NonceB64)
	if err != nil {
		return nil, fmt.Errorf("vault: corrupt nonce: %w", err)
	}
	ct, err := base64.StdEncoding.DecodeString(rec.CiphertextB64)
	if err != nil {
		return nil, fmt.Errorf("vault: corrupt ciphertext: %w", err)
	}
	pt, err := gcm.Open(nil, nonce, ct, []byte(secretPath))
	if err != nil {
		return nil, fmt.Errorf("vault: decryption failed (wrong key, wrong path or corrupt data): %w", err)
	}
	return pt, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("vault: AES cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

func (b *EnvelopeFileVaultBackend) Get(ctx context.Context, secretPath, requestID string, expiresAt time.Time) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st, err := b.load()
	if err != nil {
		return "", err
	}
	rec, ok := st.Records[secretPath]
	if !ok {
		return "", ErrSecretMaterialNotFound
	}
	pt, err := b.open(ctx, secretPath, rec)
	if err != nil {
		return "", err
	}
	clear(pt)
	return mintLeaseToken(b.signingKey, secretPath, requestID, expiresAt)
}

func (b *EnvelopeFileVaultBackend) Verify(_ context.Context, leaseToken string) (LeaseTokenInfo, error) {
	return verifyLeaseToken(b.signingKey, leaseToken)
}

func (b *EnvelopeFileVaultBackend) GetMaterial(ctx context.Context, secretPath string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st, err := b.load()
	if err != nil {
		return nil, err
	}
	rec, ok := st.Records[secretPath]
	if !ok {
		return nil, ErrSecretMaterialNotFound
	}
	return b.open(ctx, secretPath, rec)
}

func (b *EnvelopeFileVaultBackend) Put(ctx context.Context, secretPath string, material []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	st, err := b.load()
	if err != nil {
		return err
	}
	rec, err := b.seal(ctx, secretPath, material)
	if err != nil {
		return err
	}
	st.Records[secretPath] = rec
	return b.save(st)
}

func (b *EnvelopeFileVaultBackend) Rotate(ctx context.Context, secretPath string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	st, err := b.load()
	if err != nil {
		return err
	}
	if _, ok := st.Records[secretPath]; !ok {
		return ErrSecretMaterialNotFound
	}
	fresh := make([]byte, 32)
	if _, err := rand.Read(fresh); err != nil {
		return fmt.Errorf("vault: failed to generate rotated material: %w", err)
	}
	defer clear(fresh)
	rec, err := b.seal(ctx, secretPath, fresh)
	if err != nil {
		return err
	}
	st.Records[secretPath] = rec
	return b.save(st)
}

// MigrateLocalStore re-encrypts every record of a legacy local-key store
// file into the envelope format, in place. The legacy file is kept beside it
// as <path>.legacy-bak until an operator deletes it — together with the
// legacy key, which after this is the only thing that can read it. Lease
// tokens minted under the legacy key stop verifying; leases are short-lived
// and the broker simply issues new ones.
func MigrateLocalStore(ctx context.Context, storePath, legacyKeyFile string, kek KeyWrapper) (int, error) {
	legacy, err := NewLocalFileVaultBackendFromFile(storePath, legacyKeyFile)
	if err != nil {
		return 0, err
	}
	records, err := legacy.loadAll()
	if err != nil {
		return 0, err
	}
	plain := make(map[string][]byte, len(records))
	for path, rec := range records {
		pt, err := legacy.decrypt(rec)
		if err != nil {
			return 0, fmt.Errorf("vault: migrate %q: %w", path, err)
		}
		plain[path] = pt
	}
	defer func() {
		for _, pt := range plain {
			clear(pt)
		}
	}()

	if _, err := os.Stat(storePath); errors.Is(err, os.ErrNotExist) {
		_, err := NewEnvelopeFileVaultBackend(ctx, storePath, kek)
		return 0, err
	}
	if err := os.Rename(storePath, storePath+".legacy-bak"); err != nil {
		return 0, fmt.Errorf("vault: back up legacy store: %w", err)
	}
	dst, err := NewEnvelopeFileVaultBackend(ctx, storePath, kek)
	if err != nil {
		_ = os.Rename(storePath+".legacy-bak", storePath)
		return 0, err
	}
	for path, pt := range plain {
		if err := dst.Put(ctx, path, pt); err != nil {
			_ = os.Remove(storePath)
			_ = os.Rename(storePath+".legacy-bak", storePath)
			return 0, fmt.Errorf("vault: migrate %q: %w", path, err)
		}
	}
	return len(plain), nil
}

var _ Backend = (*EnvelopeFileVaultBackend)(nil)
