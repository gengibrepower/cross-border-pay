package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gengibrepower/cross-border-pay/internal/calendar"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/platform/publicid"
)

var (
	ErrUnknownMethod = errors.New("payments: unknown payment method")
	// ErrUnsupportedMethod is a known method no provider is routed to.
	ErrUnsupportedMethod   = errors.New("payments: payment method not supported yet")
	ErrUnsupportedCurrency = errors.New("payments: merchants charge in USD")
	ErrInvalidAmount       = errors.New("payments: amount must be positive")
	ErrInvalidReference    = errors.New("payments: reference must have 1 to 100 characters")
	ErrInvalidPaymentID    = errors.New("payments: invalid payment ID")
	ErrCardRequired        = errors.New("payments: a card payment needs a card token")
	ErrCardNotAllowed      = errors.New("payments: only a card payment takes a card token")
	// No card number is ever accepted or stored (section 7, PCI DSS).
	ErrInvalidCardToken = errors.New("payments: card token must be tok_ and 1 to 60 letters, digits or underscores")
)

var ChargeCurrency = money.USD

// MerchantID is payments' own name for the merchant a payment belongs to, so this
// module does not import merchants; cmd/api converts between the two.
type MerchantID [16]byte

type PaymentID [16]byte

// String is decision 37: the UUIDv7 in Crockford base32, whose alphabet is in ASCII
// order, so IDs sort by creation time as strings too.
func (id PaymentID) String() string { return publicid.Encode("pay_", id) }

func ParsePaymentID(s string) (PaymentID, error) {
	id, ok := publicid.Parse("pay_", s)
	if !ok {
		return PaymentID{}, fmt.Errorf("%w: %q", ErrInvalidPaymentID, s)
	}
	return id, nil
}

type Method string

const (
	Pix    Method = "pix"
	Boleto Method = "boleto"
	Card   Method = "card"
)

// PixQuoteValidity is decision 44 for PIX.
const PixQuoteValidity = 30 * time.Minute

// CardQuoteValidity is for automatic capture (decision 18), where the quote is used as
// the payment is created; a declined card leaves it pending to expire as a PIX would.
const CardQuoteValidity = 30 * time.Minute

// CardAuthorizationValidity is decision 52. A separate capture keeps the quote that
// long: the rate is locked at authorization, and the capture only says how much to take.
const CardAuthorizationValidity = 7 * 24 * time.Hour

type CardInput struct {
	Token         string
	ManualCapture bool
}

// ParseMethod accepts every known method; whether one is offered is the providers' call.
func ParseMethod(s string) (Method, error) {
	switch m := Method(s); m {
	case Pix, Boleto, Card:
		return m, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownMethod, s)
	}
}

type Draft struct {
	merchant  MerchantID
	method    Method
	amount    money.Money
	reference string
	card      CardInput
	payer     Payer
	due       calendar.Date
}

func NewDraft(merchant MerchantID, method string, amount money.Money, reference string, card CardInput, payer PayerInput) (Draft, error) {
	m, err := ParseMethod(method)
	if err != nil {
		return Draft{}, err
	}
	if err := checkCard(m, card); err != nil {
		return Draft{}, err
	}
	p, err := checkPayer(m, payer)
	if err != nil {
		return Draft{}, err
	}
	if amount.Currency() != ChargeCurrency {
		return Draft{}, fmt.Errorf("%w: got %s", ErrUnsupportedCurrency, amount.Currency())
	}
	if amount.Amount() <= 0 {
		return Draft{}, fmt.Errorf("%w: got %s", ErrInvalidAmount, amount)
	}
	if n := utf8.RuneCountInString(reference); n < 1 || n > 100 {
		return Draft{}, fmt.Errorf("%w: got %d", ErrInvalidReference, n)
	}
	return Draft{merchant: merchant, method: m, amount: amount, reference: reference, card: card, payer: p}, nil
}

func checkPayer(m Method, in PayerInput) (Payer, error) {
	switch {
	case m != Boleto && in != (PayerInput{}):
		return Payer{}, ErrPayerNotAllowed
	case m != Boleto:
		return Payer{}, nil
	case in == (PayerInput{}):
		return Payer{}, ErrPayerRequired
	}
	return NewPayer(in)
}

func checkCard(m Method, card CardInput) error {
	switch {
	case m != Card && card != (CardInput{}):
		return ErrCardNotAllowed
	case m != Card:
		return nil
	case card.Token == "":
		return ErrCardRequired
	case !ValidCardToken(card.Token):
		return ErrInvalidCardToken
	}
	return nil
}

func ValidCardToken(token string) bool {
	rest, ok := strings.CutPrefix(token, "tok_")
	if !ok || len(rest) < 1 || len(rest) > 60 {
		return false
	}
	for _, c := range rest {
		letter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		if !letter && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

func (d *Draft) Merchant() MerchantID { return d.merchant }

func (d *Draft) Method() Method { return d.method }

func (d *Draft) Amount() money.Money { return d.amount }

func (d *Draft) Reference() string { return d.reference }

func (d *Draft) Card() CardInput { return d.card }

// Boleto's due date is set by Schedule.
func (d *Draft) Boleto() BoletoInput { return BoletoInput{Payer: d.payer, Due: d.due} }

// Schedule returns how long the payment may wait for its payer, and sets a boleto's
// due date on the draft.
func (d *Draft) Schedule(cal *calendar.Calendar, now time.Time) time.Duration {
	switch {
	case d.method == Boleto:
		due, until := BoletoTerms(cal, now)
		d.due = due
		return until.Sub(now)
	case d.method == Card && d.card.ManualCapture:
		return CardAuthorizationValidity
	case d.method == Card:
		return CardQuoteValidity
	default:
		return PixQuoteValidity
	}
}

// Payment's status only changes through Transition.
type Payment struct {
	id        PaymentID
	merchant  MerchantID
	status    Status
	method    Method
	amount    money.Money
	reference string
	createdAt time.Time
}

// Restore rebuilds a Payment from storage; only adapters call it.
func Restore(id PaymentID, merchant MerchantID, status Status, method Method, amount money.Money, reference string, createdAt time.Time) Payment {
	return Payment{id: id, merchant: merchant, status: status, method: method, amount: amount, reference: reference, createdAt: createdAt}
}

func (p *Payment) ID() PaymentID { return p.id }

func (p *Payment) Merchant() MerchantID { return p.merchant }

func (p *Payment) Status() Status { return p.status }

func (p *Payment) Method() Method { return p.method }

func (p *Payment) Amount() money.Money { return p.amount }

func (p *Payment) Reference() string { return p.reference }

func (p *Payment) CreatedAt() time.Time { return p.createdAt }
