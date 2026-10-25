package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/idempotency/domain"
)

var (
	ErrInProgress = errors.New("idempotency: a request with this key is in progress")
	ErrKeyReused  = errors.New("idempotency: key reused with a different request")
	ErrLeaseLost  = errors.New("idempotency: lease lost to a resumed request")
)

type Clock interface {
	Now() time.Time
}

// Acquired is either a key to run the request under or a response to replay.
type Acquired struct {
	Held   *domain.Held
	Replay *domain.Response
}

type Store interface {
	// Acquire inserts the key or, if it exists, applies domain.Decide to it under a
	// row lock. It returns ErrInProgress or ErrKeyReused for those outcomes.
	Acquire(ctx context.Context, req *domain.Request, token domain.LeaseToken, now time.Time) (Acquired, error)
	// Complete returns ErrLeaseLost if another request has resumed the key since.
	Complete(ctx context.Context, held *domain.Held, resp *domain.Response) error
	// Fail deletes the key if the request never got past domain.Started. Otherwise it
	// keeps the key and ends the lease at now, so a retry resumes from the recovery
	// point at once instead of starting over.
	Fail(ctx context.Context, held *domain.Held, now time.Time) (released bool, err error)
}

type Service struct {
	store  Store
	clock  Clock
	random io.Reader
}

func NewService(store Store, clock Clock, random io.Reader) *Service {
	return &Service{store: store, clock: clock, random: random}
}

func (s *Service) Begin(ctx context.Context, req *domain.Request) (Acquired, error) {
	var token domain.LeaseToken
	if _, err := io.ReadFull(s.random, token[:]); err != nil {
		return Acquired{}, fmt.Errorf("idempotency: lease token: %w", err)
	}
	a, err := s.store.Acquire(ctx, req, token, s.clock.Now())
	if err != nil {
		return Acquired{}, fmt.Errorf("idempotency: begin: %w", err)
	}
	return a, nil
}

func (s *Service) Complete(ctx context.Context, held *domain.Held, resp *domain.Response) error {
	if err := s.store.Complete(ctx, held, resp); err != nil {
		return fmt.Errorf("idempotency: complete: %w", err)
	}
	return nil
}

// Fail is for a request that ended in a 5xx (decision 16).
func (s *Service) Fail(ctx context.Context, held *domain.Held) (released bool, err error) {
	released, err = s.store.Fail(ctx, held, s.clock.Now())
	if err != nil {
		return false, fmt.Errorf("idempotency: fail: %w", err)
	}
	return released, nil
}
