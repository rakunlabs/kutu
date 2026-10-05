package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

const metaRegistryTokenKey = "registry_token_key"

// RegistryTokenKey returns the HMAC key for short-lived registry bearer
// tokens (Docker /v2/token), generating and persisting it on first use so
// every replica signs with the same key.
func (s *Service) RegistryTokenKey(ctx context.Context) ([]byte, error) {
	var stored string
	if ok, err := s.store.GetMeta(ctx, metaRegistryTokenKey, &stored); err != nil {
		return nil, err
	} else if ok && stored != "" {
		return hex.DecodeString(stored)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate registry token key: %w", err)
	}
	if err := s.store.SetMeta(ctx, metaRegistryTokenKey, hex.EncodeToString(b)); err != nil {
		return nil, err
	}
	// Re-read so concurrent first boots converge on whichever write won.
	if _, err := s.store.GetMeta(ctx, metaRegistryTokenKey, &stored); err != nil {
		return nil, err
	}
	return hex.DecodeString(stored)
}
