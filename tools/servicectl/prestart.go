// On-disk material and paths that compose supplied through named volumes.
//
// Four services were configured with absolute container paths -- /keys,
// /data/documents, /data/ca, /tmp/secret_store.local -- each backed by a Docker
// named volume. Without Docker those paths do not exist, and on Windows they
// resolve against the current drive root, so the failure is either a refusal to
// start or, worse, a service quietly writing to C:\data.
//
// This is the same class of problem as the Docker DNS names in the registry, and
// it gets the same treatment: the launcher owns a directory per service under
// .servicectl/data and rewrites the path to point there. One of the volumes also
// had a sidecar container populating it -- identity-svc-keygen ran
// `openssl genrsa` into /keys -- so that is generated here too, with crypto/rsa
// rather than by shelling out, which means openssl need not be on PATH.
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
)

// volumePath describes one env variable that named a Docker volume path.
type volumePath struct {
	// Key is the environment variable holding the path.
	Key string
	// Rel is where it goes under .servicectl/data/<service>/.
	Rel string
	// Dir is true when the value is a directory the service expects to exist,
	// false when it is a file path the service opens or creates itself.
	Dir bool
}

// volumePaths is keyed by service, because the same variable name could mean
// different things elsewhere and a blanket rewrite by name would be guesswork.
var volumePaths = map[string][]volumePath{
	// identity_jwt_keys:/keys, populated by the identity-svc-keygen sidecar.
	"identity-context-svc": {{Key: "JWT_SIGNING_PRIVATE_KEY_PATH", Rel: "envelope_signing_key.pem"}},
	// document_vault_storage:/data/documents -- the blobs whose checksums the
	// service re-verifies on every read.
	"document-vault-svc": {{Key: "STORAGE_DIR", Rel: "documents", Dir: true}},
	// mtls_ca_data:/data/ca -- the issuing CA's own material.
	"mtls-management-svc": {{Key: "CA_DATA_DIR", Rel: "ca", Dir: true}},
	// /tmp/secret_store.local -- encrypted-at-rest dev secret material, which
	// .gitignore already refuses to let near a commit.
	"secret-vault-integration-svc": {{Key: "VAULT_LOCAL_STORE_PATH", Rel: "secret_store.local"}},
}

// prepare relocates a service's volume paths and creates whatever must exist
// before it starts.
//
// Called before every start rather than once at setup, so a service is startable
// from a clean checkout with no separate bootstrap step. That was the property
// the compose sidecar had, and it is worth keeping.
func (s *Supervisor) prepare(svc *Service, env map[string]string) error {
	for _, vp := range volumePaths[svc.Name] {
		// An explicit value in the global env always wins: a deployed
		// environment points these at real storage, and the launcher's own
		// directory is only the local default.
		if override := s.env.Get(vp.Key, ""); override != "" {
			env[vp.Key] = override
			continue
		}

		target := filepath.Join(s.root, ".servicectl", "data", svc.Name, vp.Rel)
		dir := target
		if !vp.Dir {
			dir = filepath.Dir(target)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("%s: creating %s: %w", vp.Key, dir, err)
		}
		if old := env[vp.Key]; old != "" && old != target {
			s.note(svc.Name, fmt.Sprintf("%s %s was a Docker volume path; using %s", vp.Key, old, target))
		}
		env[vp.Key] = target
	}

	// The envelope signing key has to exist and be a valid RSA key, not just
	// have a directory. THIS IS A THROWAWAY DEVELOPMENT KEY, exactly as the
	// compose sidecar's was: generated locally, never rotated, public half
	// served from the service's own /.well-known/jwks.json.
	if svc.Name == "identity-context-svc" {
		path := env["JWT_SIGNING_PRIVATE_KEY_PATH"]
		if path == "" {
			return nil
		}
		created, err := ensureRSAKey(path, 2048)
		if err != nil {
			return fmt.Errorf("envelope signing key: %w", err)
		}
		if created {
			s.note(svc.Name, "generated a throwaway 2048-bit envelope signing key at "+path)
		}
	}
	return nil
}

// ensureRSAKey writes a PKCS#1 RSA private key to path unless a usable one is
// already there. Reports whether it generated one.
//
// An existing file is PARSED rather than trusted, because a truncated or
// half-written key is worse than a missing one: the service fails at
// NewJWTSigner with "parse private key", which reads like a code fault rather
// than a corrupt file, and restarting never fixes it.
func ensureRSAKey(path string, bits int) (bool, error) {
	if b, err := os.ReadFile(path); err == nil {
		if block, _ := pem.Decode(b); block != nil {
			if _, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
				return false, nil
			}
			if _, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
				return false, nil
			}
		}
		// Unusable: fall through and replace it.
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return false, err
	}
	encoded := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	// Written to a temporary file and renamed, so a start interrupted mid-write
	// cannot leave behind the truncated key described above.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, encoded, 0o600); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return false, err
	}
	return true, nil
}
