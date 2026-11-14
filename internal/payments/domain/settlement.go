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
	// ErrDebtLimit refuses a new payment from a merchant owing more than DebtLimit.
	ErrDebtLimit = errors.New("payments: merchant is over its debt limit")
)

// DebtLimit is decision 27's, in USD cents: a merchant whose available, pending and
// reserved money together is below -US$ 1,000.00 takes no new payment until money in
// or a payment from the merchant brings it back (decision 52, illustrative).
const DebtLimit = 100_000

// OverDebtLimit tells whether a merchant holding position with the platform, its
// payable and its reserve together, may not take a new payment.
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

// SettlementPolicy is when money becomes the merchant's to be paid out: the daily
// cutoff in local time (decision 20) and each method's days of availability after the
// payment (decision 25), and how much of each payout is held against chargebacks
// (decision 26). Days are calendar days.
type SettlementPolicy struct {
	Location *time.Location
	Days     map[Method]int
	// ReserveBasisPoints of every payout is held in the merchant's reserve, for
	// ReserveDays after its cutoff.
	ReserveBasisPoints int64
	ReserveDays        int
}

// DefaultSettlementPolicy is Brazil's: cutoff in America/Sao_Paulo, PIX D+1, boleto
// D+2, card D+30, and 10% of each settlement held for 90 days. The numbers are
// illustrative (decisions 25 and 52).
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

// ReserveHold is the share of available held in the reserve, rounded down: the cent
// a rounding leaves goes to the merchant now rather than in 90 days.
func (p *SettlementPolicy) ReserveHold(available int64) int64 {
	if available <= 0 {
		return 0
	}
	return available/10_000*p.ReserveBasisPoints + available%10_000*p.ReserveBasisPoints/10_000
}

// ReserveUntil is the cutoff date a hold made at cutoff is released on.
func (p *SettlementPolicy) ReserveUntil(cutoff time.Time) time.Time {
	return cutoff.AddDate(0, 0, p.ReserveDays)
}

// ReserveRelease is what goes back to the merchant's payable when holds become due.
// held is every hold not released yet, due the part of it whose date has come, and
// balance what the reserve account holds: less than held when a negative balance
// used the reserve (decision 27). That use is taken from the oldest holds first, the
// ones due soonest, so a release is what is due less what was used, never below zero.
func ReserveRelease(held, due, balance int64) int64 {
	used := held - balance
	return min(max(due-used, 0), balance)
}

// ReserveCover is what the reserve gives a merchant whose available amount is
// negative: the debt, up to what the reserve holds (decision 27).
func ReserveCover(available, reserve int64) int64 {
	if available >= 0 || reserve <= 0 {
		return 0
	}
	return min(-available, reserve)
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
// payments that became available for it. Amount is what was paid out and Reserved what
// was held back until ReserveUntil (decision 26); Amount plus Reserved was available.
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

// SettlementPosting takes what was available out of the merchant's payable: the
// amount into settlements paid, the counterpart of payouts (section 3.3), and the hold
// into the merchant's reserve.
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

// ReserveCoverReference names one use of the reserve; a cutoff can have several,
// when the merchant goes negative again after the first.
func ReserveCoverReference(merchant MerchantID, id [16]byte) string {
	return fmt.Sprintf("reserve_cover:%x:%x", merchant, id)
}

// ReservePosting moves amount from the merchant's reserve back to the payable, for a
// release or a cover.
func ReservePosting(reference string, amount money.Money, reserve, payable ledger.AccountID) (ledger.Transaction, error) {
	if amount.Amount() <= 0 {
		return ledger.Transaction{}, fmt.Errorf("%w: %s moves a positive amount, got %s", ErrInvalidAmount, reference, amount)
	}
	return postLegs(reference,
		leg{reserve, amount, true},
		leg{payable, amount, false})
}

// Balance is the merchant's money held by the platform (section 4.9): Available is what
// a settlement run now would pay, Pending what is not available yet (decision 25), and
// Reserved what is held against chargebacks (decision 26). Available is negative when
// refunds or chargebacks of settled money exceed what came in since and the reserve
// (decision 27).
type Balance struct {
	Available money.Money
	Pending   money.Money
	Reserved  money.Money
}
