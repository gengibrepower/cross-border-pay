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

var (
	ErrInvalidSettlementID = errors.New("payments: invalid settlement ID")
	ErrDebtLimit           = errors.New("payments: merchant is over its debt limit")
)

// DebtLimit is in USD cents (decision 27; the number is illustrative, decision 52).
const DebtLimit = 100_000

// position is the merchant's payable and reserve together.
func OverDebtLimit(position int64) bool { return position < -DebtLimit }

type SettlementID [16]byte

func (id SettlementID) String() string { return publicid.Encode("stl_", id) }

func ParseSettlementID(s string) (SettlementID, error) {
	id, ok := publicid.Parse("stl_", s)
	if !ok {
		return SettlementID{}, fmt.Errorf("%w: %q", ErrInvalidSettlementID, s)
	}
	return id, nil
}

// SettlementPolicy: the cutoff is local midnight (decision 20), Days are calendar days
// of availability per method (decision 25), and the reserve is held against
// chargebacks (decision 26).
type SettlementPolicy struct {
	Location           *time.Location
	Days               map[Method]int
	ReserveBasisPoints int64
	ReserveDays        int
}

// The numbers are illustrative (decisions 25 and 52).
func DefaultSettlementPolicy() (SettlementPolicy, error) {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		return SettlementPolicy{}, fmt.Errorf("payments: settlement zone: %w", err)
	}
	return SettlementPolicy{
		Location: loc, Days: map[Method]int{Pix: 1, Boleto: 2, Card: 30},
		ReserveBasisPoints: 1000, ReserveDays: 90,
	}, nil
}

// Rounded down: the leftover cent goes to the merchant now rather than in 90 days.
// Dividing by 10_000 first keeps available * basis points from overflowing.
func (p *SettlementPolicy) ReserveHold(available int64) int64 {
	if available <= 0 {
		return 0
	}
	return available/10_000*p.ReserveBasisPoints + available%10_000*p.ReserveBasisPoints/10_000
}

func (p *SettlementPolicy) ReserveUntil(cutoff time.Time) time.Time {
	return cutoff.AddDate(0, 0, p.ReserveDays)
}

// ReserveRelease: held is every hold not released yet, due the part of it whose date
// has come, and balance what the reserve holds, less than held when a debt used it
// (decision 27). A use is taken from the oldest holds, the ones due first, so a release
// is what is due less what was used, never below zero.
func ReserveRelease(held, due, balance int64) int64 {
	used := held - balance
	return min(max(due-used, 0), balance)
}

// ReserveCover pays a negative available from the reserve, up to what it holds
// (decision 27).
func ReserveCover(available, reserve int64) int64 {
	if available >= 0 || reserve <= 0 {
		return 0
	}
	return min(-available, reserve)
}

// CutoffDate is the local date at now, as midnight UTC.
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

// Settlement: Amount was paid out and Reserved held until ReserveUntil (decision 26);
// together they are what was available at the cutoff.
type Settlement struct {
	ID           SettlementID
	Merchant     MerchantID
	CutoffDate   time.Time
	Amount       money.Money
	Reserved     money.Money
	ReserveUntil time.Time
	Payments     []PaymentID
	CreatedAt    time.Time
}

func SettlementReference(id SettlementID) string { return "settlement:" + id.String() }

func SettlementPosting(id SettlementID, amount, hold money.Money, payable, paid, reserve ledger.AccountID) (ledger.Transaction, error) {
	if amount.Amount() <= 0 || hold.IsNegative() {
		return ledger.Transaction{}, fmt.Errorf("%w: a settlement pays a positive amount and holds none below zero, got %s and %s", ErrInvalidAmount, amount, hold)
	}
	available, err := amount.Add(hold)
	if err != nil {
		return ledger.Transaction{}, fmt.Errorf("payments: settlement %s: %w", id, err)
	}
	return postLegs(SettlementReference(id),
		leg{payable, available, true},
		leg{paid, amount, false},
		leg{reserve, hold, false})
}

// ReserveReleaseReference is unique per merchant and cutoff: holds due by a cutoff are
// marked released with it, so no later run of that cutoff finds them due again.
func ReserveReleaseReference(merchant MerchantID, cutoff time.Time) string {
	return fmt.Sprintf("reserve_release:%x:%s", merchant, cutoff.Format(time.DateOnly))
}

// A cutoff can use the reserve more than once, so the reference is per use.
func ReserveCoverReference(merchant MerchantID, id [16]byte) string {
	return fmt.Sprintf("reserve_cover:%x:%x", merchant, id)
}

func ReservePosting(reference string, amount money.Money, reserve, payable ledger.AccountID) (ledger.Transaction, error) {
	if amount.Amount() <= 0 {
		return ledger.Transaction{}, fmt.Errorf("%w: %s moves a positive amount, got %s", ErrInvalidAmount, reference, amount)
	}
	return postLegs(reference,
		leg{reserve, amount, true},
		leg{payable, amount, false})
}

// Balance is section 4.9. Available goes negative when refunds or chargebacks of
// settled money exceed what came in since and the reserve (decision 27).
type Balance struct {
	Available money.Money
	Pending   money.Money
	Reserved  money.Money
}
