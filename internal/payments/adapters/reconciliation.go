package adapters

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	ledgeradapters "github.com/gengibrepower/cross-border-pay/internal/ledger/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/adapters/paymentsdb"
	"github.com/gengibrepower/cross-border-pay/internal/payments/app"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

// MovementKind has the values of the providers' statements.
type MovementKind string

const (
	Received    MovementKind = "received"
	Refunded    MovementKind = "refunded"
	ChargedBack MovementKind = "charged_back"
	Reversed    MovementKind = "reversed"
)

// Movement is money the gateway posted on the provider's receivable. Key is the ID the
// gateway gave the provider: an attempt's or a refund's; a chargeback has none.
type Movement struct {
	Kind       MovementKind
	Key        string
	ExternalID string
	Amount     money.Money
	At         time.Time
}

const receivables = "provider_receivables:BRL"

// Movements are dated by when the gateway learned them. The amount is what the ledger
// posted, so a fact without its posting is no movement at all. Money held for review
// is the review queue's to report.
func (p *Postgres) Movements(ctx context.Context, provider domain.ProviderName, since, until time.Time) ([]Movement, error) {
	q := paymentsdb.New(p.pool)
	name, from, to := string(provider), pgtype.Timestamptz{Time: since, Valid: true}, pgtype.Timestamptz{Time: until, Valid: true}
	attempts, err := q.SucceededAttemptsBetween(ctx, paymentsdb.SucceededAttemptsBetweenParams{Provider: name, Since: from, Until: to})
	if err != nil {
		return nil, fmt.Errorf("payments: attempts of %s: %w", provider, err)
	}
	refunds, err := q.SucceededRefundsBetween(ctx, paymentsdb.SucceededRefundsBetweenParams{Provider: name, Since: from, Until: to})
	if err != nil {
		return nil, fmt.Errorf("payments: refunds of %s: %w", provider, err)
	}
	chargebacks, err := q.ChargebacksBetween(ctx, paymentsdb.ChargebacksBetweenParams{Provider: name, Since: from, Until: to})
	if err != nil {
		return nil, fmt.Errorf("payments: chargebacks of %s: %w", provider, err)
	}

	// Each candidate names the posting that proves it; the sign is the provider's side.
	type candidate struct {
		m         Movement
		reference string
		sign      int64
	}
	var all []candidate
	for _, a := range attempts {
		// A succeeded attempt without its confirmation is money held for review.
		all = append(all, candidate{
			Movement{Received, domain.AttemptID(a.ID.Bytes).String(), a.ExternalID.String, money.Money{}, a.UpdatedAt.Time},
			domain.ConfirmationReference(a.PaymentID.Bytes), 1,
		})
	}
	for _, r := range refunds {
		id := domain.RefundID(r.ID.Bytes)
		all = append(all, candidate{
			Movement{Refunded, id.String(), r.ExternalID.String, money.Money{}, r.UpdatedAt.Time},
			domain.RefundReference(id, domain.RefundComplete), -1,
		})
	}
	for _, c := range chargebacks {
		id := domain.ChargebackID(c.ID.Bytes)
		if !c.CreatedAt.Time.Before(since) && c.CreatedAt.Time.Before(until) {
			all = append(all, candidate{
				Movement{ChargedBack, "", c.ExternalID, money.Money{}, c.CreatedAt.Time},
				domain.ChargebackReference(id, domain.ChargebackDebit), -1,
			})
		}
		if c.Status == string(domain.Won) && c.ClosedAt.Valid && !c.ClosedAt.Time.Before(since) && c.ClosedAt.Time.Before(until) {
			all = append(all, candidate{
				Movement{Reversed, "", c.ExternalID, money.Money{}, c.ClosedAt.Time},
				domain.ChargebackReference(id, domain.ChargebackReversal), 1,
			})
		}
	}

	l := ledgeradapters.Ledger{}
	accounts, err := l.AccountsByCode(ctx, p.pool, receivables)
	if err != nil {
		return nil, fmt.Errorf("payments: chart of accounts: %w", err)
	}
	refs := make([]string, len(all))
	for i := range all {
		refs[i] = all[i].reference
	}
	posted, err := l.AmountsByReference(ctx, p.pool, accounts[receivables].ID(), refs)
	if err != nil {
		return nil, fmt.Errorf("payments: %w", err)
	}
	var out []Movement
	for i := range all {
		c := &all[i]
		if amount, ok := posted[c.reference]; ok {
			c.m.Amount = money.New(amount*c.sign, money.BRL)
			out = append(out, c.m)
		}
	}
	return out, nil
}

// SettleReceived is a provider's payment the gateway never learned of. With an attempt
// of this provider it goes the way the provider's own event would; without one, it is
// held for a person, as section 4.4 holds money that confirms nothing. False is money
// that contradicts the attempt, which only a person can place.
func (p *Postgres) SettleReceived(ctx context.Context, provider domain.ProviderName, m *Movement, now time.Time) (settled bool, err error) {
	if m.ExternalID == "" || m.Amount.Amount() <= 0 || m.Amount.Currency() != money.BRL {
		return false, fmt.Errorf("payments: unpostable statement line %+v", m)
	}
	err = pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		u := domain.AttemptUpdate{Event: domain.Succeed, ExternalID: m.ExternalID, Amount: m.Amount, PaidAt: m.At.UTC()}
		_, err := applyPaid(ctx, tx, &p.sinks, provider, m.Key, &u, now)
		_, notOurs := domain.ParseAttemptID(m.Key)
		switch {
		case err == nil:
			settled = true
			return nil
		case notOurs != nil, errors.Is(err, app.ErrAttemptNotFound):
			settled = true
			return p.holdUnknown(ctx, tx, provider, m, now)
		case errors.Is(err, app.ErrEventDivergent):
			return nil
		default:
			return err
		}
	})
	if err != nil {
		return false, fmt.Errorf("payments: settle %s %s: %w", provider, m.ExternalID, err)
	}
	return settled, nil
}

func (p *Postgres) holdUnknown(ctx context.Context, tx pgx.Tx, provider domain.ProviderName, m *Movement, now time.Time) error {
	reference := domain.DivergenceReference(provider, m.ExternalID)
	opened, err := p.sinks.Reviews.Open(ctx, tx, &domain.ReviewCase{
		Kind: domain.MissingInSystem, Provider: provider, ExternalID: m.ExternalID, ProviderKey: m.Key,
		Amount: m.Amount, LedgerReference: reference, CreatedAt: now,
		Details: fmt.Sprintf("the provider's statement shows %s received for %q, which names no attempt of the gateway", m.Amount, m.Key),
	})
	if err != nil {
		return fmt.Errorf("review case: %w", err)
	}
	if !opened {
		return nil
	}
	return postDivergence(ctx, tx, reference, m.Amount)
}

// SettleRefunded is a refund the provider completed while the gateway still waits for
// the answer, finished as that answer would. False is a refund the gateway does not
// have pending for that amount.
func (p *Postgres) SettleRefunded(ctx context.Context, provider domain.ProviderName, m *Movement, now time.Time) (bool, error) {
	id, ok := domain.ParseRefundID(m.Key)
	if !ok {
		return false, nil
	}
	row, err := paymentsdb.New(p.pool).GetRefundByID(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("payments: refund %s: %w", id, err)
	}
	if row.Provider != string(provider) || row.PayerAmount != m.Amount.Amount() || row.PayerCurrency != m.Amount.Currency().Code() {
		return false, nil
	}
	if p.sinks.Rates == nil {
		return false, errors.New("payments: settling a refund needs today's rate")
	}
	rate, err := p.sinks.Rates.Rate(ctx, domain.ChargeCurrency, money.BRL)
	if err != nil {
		return false, fmt.Errorf("payments: rate: %w", err)
	}
	refund, _, err := p.ApplyRefund(ctx, id, &domain.RefundUpdate{Event: domain.SucceedRefund, ExternalID: m.ExternalID}, rate, now)
	if errors.Is(err, domain.ErrInvalidTransition) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("payments: settle refund %s: %w", id, err)
	}
	return refund.Status() == domain.RefundSucceeded, nil
}
