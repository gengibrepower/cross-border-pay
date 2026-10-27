package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	fxadapters "github.com/gengibrepower/cross-border-pay/internal/fx/adapters"
	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	idempotencyadapters "github.com/gengibrepower/cross-border-pay/internal/idempotency/adapters"
	idempotency "github.com/gengibrepower/cross-border-pay/internal/idempotency/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/adapters/paymentsdb"
	"github.com/gengibrepower/cross-border-pay/internal/payments/app"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
)

// Outbox stores events for merchants in the caller's transaction (section 4.5). It is
// the events module's, plugged in by cmd so that payments does not import it.
type Outbox interface {
	Append(ctx context.Context, tx pgx.Tx, e *domain.MerchantEvent) error
}

// Reviews opens review cases in the caller's transaction (section 4.4), for the
// reconciliation module, plugged in like Outbox. opened is false for a charge that
// already has one.
type Reviews interface {
	Open(ctx context.Context, tx pgx.Tx, c *domain.ReviewCase) (opened bool, err error)
}

// Sinks are the other modules' writers payments uses inside its own transactions.
type Sinks struct {
	Outbox  Outbox
	Reviews Reviews
}

type Postgres struct {
	pool  *pgxpool.Pool
	sinks Sinks
}

func NewPostgres(pool *pgxpool.Pool, sinks Sinks) *Postgres {
	return &Postgres{pool: pool, sinks: sinks}
}

func (p *Postgres) Create(ctx context.Context, held *idempotency.Held, d *domain.Draft, q *fx.Quote, provider domain.ProviderName) (app.Priced, error) {
	var attempt paymentsdb.PaymentAttempt
	var id pgtype.UUID
	err := postgres.WithTx(ctx, p.pool, pgx.ReadCommitted, func(ctx context.Context, tx pgx.Tx) error {
		quoteID, err := fxadapters.SaveQuote(ctx, tx, q)
		if err != nil {
			return fmt.Errorf("quote: %w", err)
		}
		queries := paymentsdb.New(tx)
		id, err = queries.InsertPayment(ctx, paymentsdb.InsertPaymentParams{
			MerchantID: pgtype.UUID{Bytes: d.Merchant(), Valid: true},
			QuoteID:    pgtype.UUID{Bytes: quoteID, Valid: true},
			Status:     string(domain.Pending),
			Method:     string(d.Method()),
			Amount:     d.Amount().Amount(),
			Currency:   d.Amount().Currency().Code(),
			Reference:  d.Reference(),
			CreatedAt:  pgtype.Timestamptz{Time: q.CreatedAt(), Valid: true},
			// The expiry job's first look is when the quote expires.
			ExpiryCheckAt: pgtype.Timestamptz{Time: q.ExpiresAt(), Valid: true},
		})
		if err != nil {
			return fmt.Errorf("insert payment: %w", err)
		}
		attempt, err = insertAttempt(ctx, queries, id.Bytes, provider, q.CreatedAt())
		if err != nil {
			return err
		}
		// A lost lease fails here and rolls everything back: the request that resumed
		// the key creates the payment instead.
		return idempotencyadapters.SetRecoveryPoint(ctx, tx, held, app.AttemptRequested, id.Bytes)
	})
	if err != nil {
		return app.Priced{}, fmt.Errorf("payments: store: %w", err)
	}
	a, err := restoreAttempt(&attempt)
	if err != nil {
		return app.Priced{}, err
	}
	return app.Priced{
		Payment: domain.Restore(id.Bytes, d.Merchant(), domain.Pending, d.Method(), d.Amount(), d.Reference(), q.CreatedAt()),
		Quote:   *q,
		Attempt: &a,
	}, nil
}

// Get and List read the payments, then their quotes and latest attempts, outside a
// transaction. A quote never changes; an attempt may move between the reads, and then
// the newer status is the one returned, as a later GET would.
func (p *Postgres) Get(ctx context.Context, merchant domain.MerchantID, id domain.PaymentID) (app.Priced, error) {
	row, err := paymentsdb.New(p.pool).GetPayment(ctx, paymentsdb.GetPaymentParams{
		MerchantID: pgtype.UUID{Bytes: merchant, Valid: true},
		ID:         pgtype.UUID{Bytes: id, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Priced{}, app.ErrPaymentNotFound
	}
	if err != nil {
		return app.Priced{}, fmt.Errorf("payments: get: %w", err)
	}
	priced, err := p.withDetails(ctx, []paymentsdb.Payment{row})
	if err != nil {
		return app.Priced{}, err
	}
	return priced[0], nil
}

func (p *Postgres) List(ctx context.Context, merchant domain.MerchantID, q app.ListQuery) ([]app.Priced, error) {
	rows, err := paymentsdb.New(p.pool).ListPayments(ctx, paymentsdb.ListPaymentsParams{
		MerchantID: pgtype.UUID{Bytes: merchant, Valid: true},
		Before:     pgtype.UUID{Bytes: q.Before, Valid: q.Before != domain.PaymentID{}},
		Status:     pgtype.Text{String: string(q.Status), Valid: q.Status != ""},
		MaxRows:    int64(q.Limit),
	})
	if err != nil {
		return nil, fmt.Errorf("payments: list: %w", err)
	}
	return p.withDetails(ctx, rows)
}

func (p *Postgres) withDetails(ctx context.Context, rows []paymentsdb.Payment) ([]app.Priced, error) {
	quoteIDs := make([]fx.QuoteID, len(rows))
	paymentIDs := make([]pgtype.UUID, len(rows))
	for i := range rows {
		quoteIDs[i] = rows[i].QuoteID.Bytes
		paymentIDs[i] = rows[i].ID
	}
	quotes, err := fxadapters.LoadQuotes(ctx, p.pool, quoteIDs)
	if err != nil {
		return nil, fmt.Errorf("payments: %w", err)
	}
	attemptRows, err := paymentsdb.New(p.pool).LatestAttempts(ctx, paymentIDs)
	if err != nil {
		return nil, fmt.Errorf("payments: latest attempts: %w", err)
	}
	attempts := make(map[[16]byte]*domain.PaymentAttempt, len(attemptRows))
	for i := range attemptRows {
		a, err := restoreAttempt(&attemptRows[i])
		if err != nil {
			return nil, err
		}
		attempts[attemptRows[i].PaymentID.Bytes] = &a
	}

	priced := make([]app.Priced, len(rows))
	for i := range rows {
		payment, err := restorePayment(&rows[i])
		if err != nil {
			return nil, fmt.Errorf("payments: stored payment %x: %w", rows[i].ID.Bytes, err)
		}
		quote, ok := quotes[rows[i].QuoteID.Bytes]
		if !ok {
			return nil, fmt.Errorf("payments: quote %x of payment %s is missing", rows[i].QuoteID.Bytes, payment.ID())
		}
		priced[i] = app.Priced{Payment: payment, Quote: quote, Attempt: attempts[rows[i].ID.Bytes]}
	}
	return priced, nil
}

func restorePayment(row *paymentsdb.Payment) (domain.Payment, error) {
	status, err := domain.ParseStatus(row.Status)
	if err != nil {
		return domain.Payment{}, fmt.Errorf("status: %w", err)
	}
	currency, err := money.ParseCurrency(row.Currency)
	if err != nil {
		return domain.Payment{}, fmt.Errorf("currency: %w", err)
	}
	// The CHECK on payments.method allows only known methods; ParseMethod would refuse
	// boleto and card, which are known but not offered yet.
	method := domain.Method(row.Method)
	return domain.Restore(row.ID.Bytes, row.MerchantID.Bytes, status, method,
		money.New(row.Amount, currency), row.Reference, row.CreatedAt.Time.UTC()), nil
}
