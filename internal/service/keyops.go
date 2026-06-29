package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"

	"github.com/rakunlabs/kutu/internal/secret/crypto"
	"github.com/rakunlabs/kutu/internal/secret/keymgr"
)

// Server-key lifecycle operations powering the
// /api/v1/key/{status,initialize,unlock,lock,rotate} endpoints. Mirrors
// HashiCorp Vault's seal/unseal split. The verifier blob lives in
// kutu_meta; per-row registry credentials are sealed by the storage
// layer under the live key.

// SetKeyManager wires the at-rest key manager into the service.
func (s *Service) SetKeyManager(mgr *keymgr.Manager) { s.keyManager = mgr }

// KeyManager returns the wired manager, or nil.
func (s *Service) KeyManager() *keymgr.Manager { return s.keyManager }

// KeyStatus is the response shape for GET /api/v1/key/status.
type KeyStatus struct {
	Initialized bool `json:"initialized"`
	Unlocked    bool `json:"unlocked"`
}

func (s *Service) getVerifier(ctx context.Context) ([]byte, error) {
	var v []byte
	if _, err := s.store.GetMeta(ctx, metaEncVerifier, &v); err != nil {
		return nil, err
	}
	return v, nil
}

func (s *Service) setVerifier(ctx context.Context, v []byte) error {
	return s.store.SetMeta(ctx, metaEncVerifier, v)
}

// GetKeyStatus combines the on-disk verifier presence with the in-memory
// unlock state.
func (s *Service) GetKeyStatus(ctx context.Context) (*KeyStatus, error) {
	verifier, err := s.getVerifier(ctx)
	if err != nil {
		return nil, fmt.Errorf("read verifier: %w", err)
	}
	st := &KeyStatus{Initialized: len(verifier) > 0}
	if s.keyManager != nil {
		st.Unlocked = s.keyManager.IsUnlocked()
		if st.Initialized {
			s.keyManager.MarkInitialized()
		}
	}
	return st, nil
}

var verifierMagic = []byte("KUTU_KEY_VERIFIER_v1|")

func verifierPlaintext() ([]byte, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return nil, fmt.Errorf("verifier randomness: %w", err)
	}
	out := make([]byte, 0, len(verifierMagic)+len(random))
	out = append(out, verifierMagic...)
	out = append(out, random...)
	return out, nil
}

func deriveKeyMaterial(passphrase string) []byte {
	sum := sha256.Sum256([]byte(passphrase))
	return sum[:]
}

// InitializeServerKey writes the first verifier and leaves the manager
// UNLOCKED with the new key. Fails with ErrConflict if already set.
func (s *Service) InitializeServerKey(ctx context.Context, passphrase string) error {
	if s.keyManager == nil {
		return fmt.Errorf("key manager not configured: %w", ErrInternal)
	}
	if passphrase == "" {
		return fmt.Errorf("key is required: %w", ErrBadRequest)
	}
	verifier, err := s.getVerifier(ctx)
	if err != nil {
		return fmt.Errorf("read verifier: %w", err)
	}
	if len(verifier) > 0 {
		return fmt.Errorf("server is already initialized: %w", ErrConflict)
	}

	enc, err := crypto.NewChaCha20(deriveKeyMaterial(passphrase))
	if err != nil {
		return fmt.Errorf("build encryptor: %w", err)
	}
	plain, err := verifierPlaintext()
	if err != nil {
		return err
	}
	newVerifier, err := enc.Encrypt(plain)
	if err != nil {
		return fmt.Errorf("encrypt verifier: %w", err)
	}
	if err := s.keyManager.Unlock(enc); err != nil {
		return fmt.Errorf("install encryptor: %w", err)
	}
	if err := s.setVerifier(ctx, newVerifier); err != nil {
		s.keyManager.Lock()
		return fmt.Errorf("persist verifier: %w", err)
	}
	s.keyManager.MarkInitialized()
	return nil
}

// UnlockServerKey validates the passphrase against the stored verifier
// and installs the encryptor. Wrong passphrase → ErrForbidden.
func (s *Service) UnlockServerKey(ctx context.Context, passphrase string) error {
	if s.keyManager == nil {
		return fmt.Errorf("key manager not configured: %w", ErrInternal)
	}
	if passphrase == "" {
		return fmt.Errorf("key is required: %w", ErrBadRequest)
	}
	verifier, err := s.getVerifier(ctx)
	if err != nil {
		return fmt.Errorf("read verifier: %w", err)
	}
	if len(verifier) == 0 {
		return fmt.Errorf("server is not initialized; call /api/v1/key/initialize first: %w", ErrBadRequest)
	}

	enc, err := crypto.NewChaCha20(deriveKeyMaterial(passphrase))
	if err != nil {
		return fmt.Errorf("build encryptor: %w", err)
	}
	plain, err := enc.Decrypt(verifier)
	if err != nil {
		return fmt.Errorf("invalid server key: %w", ErrForbidden)
	}
	if !bytes.HasPrefix(plain, verifierMagic) {
		return fmt.Errorf("verifier format mismatch: %w", ErrForbidden)
	}
	if err := s.keyManager.Unlock(enc); err != nil {
		return fmt.Errorf("install encryptor: %w", err)
	}
	s.keyManager.MarkInitialized()
	return nil
}

// LockServerKey clears the live key.
func (s *Service) LockServerKey() error {
	if s.keyManager == nil {
		return fmt.Errorf("key manager not configured: %w", ErrInternal)
	}
	s.keyManager.Lock()
	return nil
}

// RotateServerKey verifies the old key, then re-seals the verifier and
// every registry repository credential under the new key.
func (s *Service) RotateServerKey(ctx context.Context, oldPassphrase, newPassphrase string) error {
	if s.keyManager == nil {
		return fmt.Errorf("key manager not configured: %w", ErrInternal)
	}
	if oldPassphrase == "" || newPassphrase == "" {
		return fmt.Errorf("old and new keys are required: %w", ErrBadRequest)
	}
	if oldPassphrase == newPassphrase {
		return fmt.Errorf("new key must differ from old key: %w", ErrBadRequest)
	}
	verifier, err := s.getVerifier(ctx)
	if err != nil {
		return fmt.Errorf("read verifier: %w", err)
	}
	if len(verifier) == 0 {
		return fmt.Errorf("server is not initialized: %w", ErrBadRequest)
	}

	// Verify the old key.
	oldEnc, err := crypto.NewChaCha20(deriveKeyMaterial(oldPassphrase))
	if err != nil {
		return fmt.Errorf("build old encryptor: %w", err)
	}
	plain, err := oldEnc.Decrypt(verifier)
	if err != nil {
		return fmt.Errorf("invalid old key: %w", ErrForbidden)
	}
	if !bytes.HasPrefix(plain, verifierMagic) {
		return fmt.Errorf("verifier format mismatch: %w", ErrForbidden)
	}

	// Read every repository's credentials through the CURRENT (old) key
	// BEFORE swapping. LoadRegistryTree inflates the sealed columns.
	tree, err := s.store.LoadRegistryTree(ctx)
	if err != nil {
		return fmt.Errorf("read registry for rewrap: %w", err)
	}

	// Build + install the new key, then re-seal the verifier.
	newEnc, err := crypto.NewChaCha20(deriveKeyMaterial(newPassphrase))
	if err != nil {
		return fmt.Errorf("build new encryptor: %w", err)
	}
	newVerifier, err := newEnc.Encrypt(plain)
	if err != nil {
		return fmt.Errorf("encrypt new verifier: %w", err)
	}
	if err := s.keyManager.Unlock(newEnc); err != nil {
		return fmt.Errorf("install new encryptor: %w", err)
	}
	if err := s.setVerifier(ctx, newVerifier); err != nil {
		return fmt.Errorf("persist new verifier: %w", err)
	}

	// Re-write every repository that carries credentials so the storage
	// layer re-seals them under the new key.
	if tree != nil {
		for ni := range tree.Namespaces {
			ns := &tree.Namespaces[ni]
			for ri := range ns.Repositories {
				repo := &ns.Repositories[ri]
				if repo.Auth == nil {
					continue
				}
				if repo.Auth.Password == "" && repo.Auth.Token == "" && repo.Auth.Value == "" {
					continue
				}
				if err := s.store.UpdateRepository(ctx, ns.Name, repo); err != nil {
					return fmt.Errorf("rewrap %s/%s: %w", ns.Name, repo.Name, err)
				}
			}
		}
	}
	return nil
}
