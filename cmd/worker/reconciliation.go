package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gengibrepower/cross-border-pay/internal/calendar"
	paymentsadapters "github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	payments "github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/worker"
	"github.com/gengibrepower/cross-border-pay/internal/providers/simkit"
	reconciliationadapters "github.com/gengibrepower/cross-border-pay/internal/reconciliation/adapters"
	reconciliationapp "github.com/gengibrepower/cross-border-pay/internal/reconciliation/app"
	reconciliation "github.com/gengibrepower/cross-border-pay/internal/reconciliation/domain"
)

type statementSource interface {
	Statement(ctx context.Context, date calendar.Date) (simkit.Statement, error)
}

// statements reads each simulator's statement, by the provider name the attempts keep.
type statements map[string]statementSource

func (s statements) Statement(ctx context.Context, provider string, day calendar.Date) ([]reconciliation.Line, error) {
	source, ok := s[provider]
	if !ok {
		return nil, fmt.Errorf("no statement for provider %q", provider)
	}
	st, err := source.Statement(ctx, day)
	if err != nil {
		return nil, fmt.Errorf("%s statement: %w", provider, err)
	}
	lines := make([]reconciliation.Line, len(st.Movements))
	for i, m := range st.Movements {
		lines[i] = reconciliation.Line{Kind: reconciliation.MovementKind(m.Kind), ExternalID: m.ID, Key: m.IdempotencyKey, Amount: m.Amount, At: m.At}
	}
	return lines, nil
}

// books keeps reconciliation from importing payments.
type books struct {
	payments *paymentsadapters.Postgres
}

func (b books) Movements(ctx context.Context, provider string, since, until time.Time) ([]reconciliation.Line, error) {
	ms, err := b.payments.Movements(ctx, payments.ProviderName(provider), since, until)
	if err != nil {
		return nil, fmt.Errorf("books: %w", err)
	}
	lines := make([]reconciliation.Line, len(ms))
	for i, m := range ms {
		lines[i] = reconciliation.Line{Kind: reconciliation.MovementKind(m.Kind), ExternalID: m.ExternalID, Key: m.Key, Amount: m.Amount, At: m.At}
	}
	return lines, nil
}

func (b books) Settle(ctx context.Context, provider string, l *reconciliation.Line, now time.Time) (bool, error) {
	m := paymentsadapters.Movement{Kind: paymentsadapters.MovementKind(l.Kind), Key: l.Key, ExternalID: l.ExternalID, Amount: l.Amount, At: l.At}
	var settled bool
	var err error
	switch l.Kind {
	case reconciliation.Received:
		settled, err = b.payments.SettleReceived(ctx, payments.ProviderName(provider), &m, now)
	case reconciliation.Refunded:
		settled, err = b.payments.SettleRefunded(ctx, payments.ProviderName(provider), &m, now)
	case reconciliation.ChargedBack, reconciliation.Reversed:
		// A chargeback the gateway lacks is still on its way as an event, or for a person.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("books: %w", err)
	}
	return settled, nil
}

// reconciled are the providers whose statements the daily reconciliation reads.
var reconciled = []string{string(paymentsadapters.PixSimName), string(paymentsadapters.CardSimName), string(paymentsadapters.BoletoSimName)}

func reconciliationJob(pool *pgxpool.Pool, r *reconciliationapp.Reconciler, logger *slog.Logger) worker.Job {
	runs := reconciliationadapters.NewRuns(pool)
	return worker.Every("reconciliation", reconciliationInterval, logger, func(ctx context.Context) error {
		n, err := r.RunDue(ctx, runs, reconciled)
		if n > 0 {
			logger.InfoContext(ctx, "provider days reconciled", "count", n)
		}
		if err != nil {
			return fmt.Errorf("reconciliation: %w", err)
		}
		return nil
	})
}
