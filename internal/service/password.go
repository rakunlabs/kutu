package service

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// minPasswordLength is enforced on every password set through the API.
const minPasswordLength = 8

func validatePassword(p string) error {
	if len(p) < minPasswordLength {
		return fmt.Errorf("password must be at least %d characters: %w", minPasswordLength, ErrBadRequest)
	}
	return nil
}

// ChangeOwnPassword lets a local user rotate their password after proving
// the current one. Other sessions of the user are revoked; keepSessionID
// (the caller's own session) survives.
func (s *Service) ChangeOwnPassword(ctx context.Context, userID, current, next, keepSessionID string) error {
	if userID == "" {
		return fmt.Errorf("no user in context: %w", ErrUnauthorized)
	}
	user, err := s.store.Users().Get(ctx, userID)
	if err != nil {
		return err
	}
	if user.PasswordHash == "" {
		return fmt.Errorf("account has no local password: %w", ErrForbidden)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(current)); err != nil {
		return fmt.Errorf("current password is incorrect: %w", ErrUnauthorized)
	}
	if err := validatePassword(next); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(next), passwordCost)
	if err != nil {
		return fmt.Errorf("hashing password: %w", err)
	}
	user.PasswordHash = string(hash)
	user.UpdatedAt = time.Now()
	if err := s.store.Users().Update(ctx, user); err != nil {
		return err
	}

	sessions, err := s.store.Sessions().ListByUserID(ctx, userID)
	if err != nil {
		return nil
	}
	for _, sess := range sessions {
		if sess.ID != keepSessionID {
			_ = s.store.Sessions().Delete(ctx, sess.ID)
		}
	}
	return nil
}
