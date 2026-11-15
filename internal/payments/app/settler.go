package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

const SettleBatch = 100

type SettlementStore interface {
	// SettlementCandidates pages by merchant ID, ascending after after.
	SettlementCandidates(ctx context.Context, cutoff time.Time, after domain.MerchantID, limit int) ([]domain.MerchantID, error)
	// Settle: settled is false when nothing was due or the cutoff was already settled.
	Settle(ctx context.Context, merchant domain.MerchantID, policy *domain.SettlementPolicy, now time.Time) (domain.Settlement, bool, error)
}

// Settler is section 4.7. Running it again, or on two workers at once, pays nothing
// more: the cutoff is unique per merchant.
type Settler struct {
	store  SettlementStore
	policy domain.SettlementPolicy
	clock  Clock
	logger *slog.Logger
}

func NewSettler(store SettlementStore, policy domain.SettlementPolicy, clock Clock, logger *slog.Logger) *Settler {
	return &Settler{store: store, policy: policy, clock: clock, logger: logger}
}

// Settle returns how many were paid. One merchant failing does not stop the others.
func (s *Settler) Settle(ctx context.Context) (int, error) {
	now := s.clock.Now()
	cutoff := s.policy.CutoffDate(now)
	var (
		after domain.MerchantID
		paid  int
		errs  []error
	)
	for {
		merchants, err := s.store.SettlementCandidates(ctx, cutoff, after, SettleBatch)
		if err != nil {
			return paid, errors.Join(append(errs, fmt.Errorf("payments: settlement candidates: %w", err))...)
		}
		for _, m := range merchants {
			if ctx.Err() != nil {
				return paid, errors.Join(append(errs, ctx.Err())...)
			}
			st, ok, err := s.store.Settle(ctx, m, &s.policy, now)
			if err != nil {
				errs = append(errs, fmt.Errorf("payments: settle: %w", err))
				continue
			}
			if ok {
				paid++
				s.logger.InfoContext(ctx, "merchant settled", "settlement", st.ID.String(),
					"cutoff", st.CutoffDate.Format(time.DateOnly), "amount", st.Amount.String(), "payments", len(st.Payments))
			}
		}
		if len(merchants) < SettleBatch {
			return paid, errors.Join(errs...)
		}
		after = merchants[len(merchants)-1]
	}
}
