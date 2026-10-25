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

type Postgres struct {
	pool *pgxpool.Pool
}

func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool}
}

func (p *Postgres) Create(ctx context.Context, held *idempotency.Held, d *domain.Draft, q *fx.Quote) (domain.Payment, error) {
	var id pgtype.UUID
	err := postgres.WithTx(ctx, p.pool, pgx.ReadCommitted, func(ctx context.Context, tx pgx.Tx) error {
		quoteID, err := fxadapters.SaveQuote(ctx, tx, q)
		if err != nil {
			return fmt.Errorf("quote: %w", err)
		}
		id, err = paymentsdb.New(tx).InsertPayment(ctx, paymentsdb.InsertPaymentParams{
			MerchantID: pgtype.UUID{Bytes: d.Merchant(), Valid: true},
			QuoteID:    pgtype.UUID{Bytes: quoteID, Valid: true},
			Status:     string(domain.Pending),
			Method:     string(d.Method()),
			Amount:     d.Amount().Amount(),
			Currency:   d.Amount().Currency().Code(),
			Reference:  d.Reference(),
			CreatedAt:  pgtype.Timestamptz{Time: q.CreatedAt(), Valid: true},
		})
		if err != nil {
			return fmt.Errorf("insert payment: %w", err)
		}
		// A lost lease fails here and rolls the payment back: the request that resumed
		// the key creates it instead.
		return idempotencyadapters.SetRecoveryPoint(ctx, tx, held, app.PaymentCreated, id.Bytes)
	})
	if err != nil {
		return domain.Payment{}, fmt.Errorf("payments: store: %w", err)
	}
	return domain.Restore(id.Bytes, d.Merchant(), domain.Pending, d.Method(), d.Amount(), d.Reference(), q.CreatedAt()), nil
}

// Get and List read the payment and then its quote outside a transaction: a quote never
// changes after it is stored, so there is nothing for the two reads to disagree on.
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
	priced, err := p.withQuotes(ctx, []paymentsdb.Payment{row})
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
	return p.withQuotes(ctx, rows)
}

func (p *Postgres) withQuotes(ctx context.Context, rows []paymentsdb.Payment) ([]app.Priced, error) {
	ids := make([]fx.QuoteID, len(rows))
	for i := range rows {
		ids[i] = rows[i].QuoteID.Bytes
	}
	quotes, err := fxadapters.LoadQuotes(ctx, p.pool, ids)
	if err != nil {
		return nil, fmt.Errorf("payments: %w", err)
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
		priced[i] = app.Priced{Payment: payment, Quote: quote}
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
