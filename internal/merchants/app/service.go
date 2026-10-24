package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/gengibrepower/cross-border-pay/internal/merchants/domain"
)

var (
	// ErrUnauthorized covers every way a key can fail, so a caller cannot tell a
	// malformed key from a revoked one or a wrong secret.
	ErrUnauthorized = errors.New("merchants: invalid API key")
	ErrKeyNotFound  = errors.New("merchants: API key not found")
	ErrInvalidName  = errors.New("merchants: name must have 1 to 200 characters")
)

type StoredKey struct {
	MerchantID domain.MerchantID
	Mode       domain.Mode
	Hash       []byte
}

type Store interface {
	// CreateMerchantWithKey writes both or neither.
	CreateMerchantWithKey(ctx context.Context, name string, key *domain.NewKey) (domain.MerchantID, error)
	// FindActiveKey returns ErrKeyNotFound for an unknown or revoked lookup.
	FindActiveKey(ctx context.Context, lookup string) (StoredKey, error)
	// RevokeKey returns ErrKeyNotFound if no active key has this lookup.
	RevokeKey(ctx context.Context, lookup string) error
}

type Service struct {
	store  Store
	random io.Reader
}

func NewService(store Store, random io.Reader) *Service {
	return &Service{store: store, random: random}
}

// CreateMerchant returns the only copy of the key that will ever exist in plaintext.
func (s *Service) CreateMerchant(ctx context.Context, name string, mode domain.Mode) (domain.MerchantID, string, error) {
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n < 1 || n > 200 {
		return domain.MerchantID{}, "", fmt.Errorf("%w: got %d", ErrInvalidName, n)
	}
	key, err := domain.GenerateKey(mode, s.random)
	if err != nil {
		return domain.MerchantID{}, "", fmt.Errorf("merchants: create: %w", err)
	}
	id, err := s.store.CreateMerchantWithKey(ctx, name, &key)
	if err != nil {
		return domain.MerchantID{}, "", fmt.Errorf("merchants: create: %w", err)
	}
	return id, key.Plaintext, nil
}

func (s *Service) Authenticate(ctx context.Context, plaintext string) (domain.Principal, error) {
	_, lookup, err := domain.ParseKey(plaintext)
	if err != nil {
		return domain.Principal{}, ErrUnauthorized
	}
	stored, err := s.store.FindActiveKey(ctx, lookup)
	if errors.Is(err, ErrKeyNotFound) {
		return domain.Principal{}, ErrUnauthorized
	}
	if err != nil {
		return domain.Principal{}, fmt.Errorf("merchants: authenticate: %w", err)
	}
	if !domain.Matches(plaintext, stored.Hash) {
		return domain.Principal{}, ErrUnauthorized
	}
	return domain.Principal{MerchantID: stored.MerchantID, Mode: stored.Mode}, nil
}

func (s *Service) RevokeKey(ctx context.Context, lookup string) error {
	if err := s.store.RevokeKey(ctx, lookup); err != nil {
		return fmt.Errorf("merchants: revoke %s: %w", lookup, err)
	}
	return nil
}
