package domain

import (
	"errors"
	"fmt"
	"time"
	_ "time/tzdata" // the cutoff's zone must load in a container without zoneinfo

	ledger "github.com/gengibrepower/cross-border-pay/internal/ledger/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/platform/publicid"
)

var ErrInvalidSettlementID = errors.New("payments: invalid settlement ID")

type SettlementID [16]byte

func (id SettlementID) String() string { return publicid.Encode("stl_", id) }

func ParseSettlementID(s string) (SettlementID, error) {
	id, ok := publicid.Parse("stl_", s)
	if !ok {
		return SettlementID{}, fmt.Errorf("%w: %q", ErrInvalidSettlementID, s)
	}
	return id, nil
}

// SettlementPolicy is when money becomes the merchant's to be paid out: the daily
// cutoff in local time (decision 20) and each method's days of availability after the
// payment (decision 25). Days are calendar days; business days come with the calendar
// of decision 21, in F8.
type SettlementPolicy struct {
	Location *time.Location
	Days     map[Method]int
}

// DefaultSettlementPolicy is Brazil's: cutoff in America/Sao_Paulo, PIX D+1, boleto
// D+2, card D+30. The numbers are illustrative (decision 25).
func DefaultSettlementPolicy() (SettlementPolicy, error) {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		return SettlementPolicy{}, fmt.Errorf("payments: settlement zone: %w", err)
	}
	return SettlementPolicy{Location: loc, Days: map[Method]int{Pix: 1, Boleto: 2, Card: 30}}, nil
}

// CutoffDate is the local date at now, as midnight UTC of that date: the cutoff a
// settlement run at now belongs to.
func (p *SettlementPolicy) CutoffDate(now time.Time) time.Time {
	y, m, d := now.In(p.Location).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// AvailableBefore is the instant before which a payment of the method is available by
// the cutoff date: one paid on local day P is available from day P+days, so by cutoff D
// when P <= D-days, that is, when it was paid before local midnight starting D-days+1.
func (p *SettlementPolicy) AvailableBefore(m Method, cutoff time.Time) time.Time {
	y, mo, d := cutoff.Date()
	return time.Date(y, mo, d-p.Days[m]+1, 0, 0, 0, 0, p.Location).UTC()
}

// Settlement is one payout to a merchant: what was available at the cutoff, with the
// payments that became available for it.
type Settlement struct {
	ID         SettlementID
	Merchant   MerchantID
	CutoffDate time.Time
	Amount     money.Money
	Payments   []PaymentID
	CreatedAt  time.Time
}

func SettlementReference(id SettlementID) string { return "settlement:" + id.String() }

// SettlementPosting pays the amount out of the merchant's payable into settlements
// paid, the counterpart of payouts (section 3.3).
func SettlementPosting(id SettlementID, amount money.Money, payable, paid ledger.AccountID) (ledger.Transaction, error) {
	if amount.Amount() <= 0 {
		return ledger.Transaction{}, fmt.Errorf("%w: a settlement pays a positive amount, got %s", ErrInvalidAmount, amount)
	}
	return postLegs(SettlementReference(id),
		leg{payable, amount, true},
		leg{paid, amount, false})
}
