package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gengibrepower/cross-border-pay/internal/platform/publicid"

	"github.com/gengibrepower/cross-border-pay/internal/money"
)

var (
	ErrUnknownMethod = errors.New("payments: unknown payment method")
	// ErrUnsupportedMethod is a known method no provider is routed to.
	ErrUnsupportedMethod   = errors.New("payments: payment method not supported yet")
	ErrUnsupportedCurrency = errors.New("payments: merchants charge in USD")
	ErrInvalidAmount       = errors.New("payments: amount must be positive")
	ErrInvalidReference    = errors.New("payments: reference must have 1 to 100 characters")
	ErrInvalidPaymentID    = errors.New("payments: invalid payment ID")
	// ErrCardRequired is a card payment without a card token, and ErrCardNotAllowed a
	// card token on a payment of another method.
	ErrCardRequired   = errors.New("payments: a card payment needs a card token")
	ErrCardNotAllowed = errors.New("payments: only a card payment takes a card token")
	// ErrInvalidCardToken is anything that is not a token, a card number included:
	// no card number is ever accepted or stored (section 7, PCI DSS).
	ErrInvalidCardToken = errors.New("payments: card token must be tok_ and 1 to 60 letters, digits or underscores")
)

// ChargeCurrency is the only currency merchants charge in; payers pay in BRL.
var ChargeCurrency = money.USD

// MerchantID is payments' own name for the merchant a payment belongs to, so this
// module does not import merchants; cmd/api converts between the two.
type MerchantID [16]byte

type PaymentID [16]byte

// String is the public ID (decision 37): "pay_" and the UUIDv7 in Crockford base32,
// whose alphabet is in ASCII order, so IDs sort by creation time as strings too.
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

// CardQuoteValidity bounds a card payment's quote. Decision 44 prices a card at the
// moment of its authorization, and with automatic capture (decision 18) the payment
// is authorized and captured as it is created, so the quote is used at once; a
// declined card leaves the payment pending and it expires as a PIX one would.
const CardQuoteValidity = 30 * time.Minute

// QuoteValidity is how long a payment of the method may wait for its payer.
func QuoteValidity(m Method) time.Duration {
	switch m {
	case Card:
		return CardQuoteValidity
	case Pix, Boleto:
		return PixQuoteValidity
	default:
		return PixQuoteValidity
	}
}

// ParseMethod accepts every method the domain knows. Whether one is offered is the
// providers' call: a method with no provider routed is ErrUnsupportedMethod there.
func ParseMethod(s string) (Method, error) {
	switch m := Method(s); m {
	case Pix, Boleto, Card:
		return m, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownMethod, s)
	}
}

// Draft is a validated request for a payment, not yet stored.
type Draft struct {
	merchant  MerchantID
	method    Method
	amount    money.Money
	reference string
	cardToken string
}

// NewDraft validates a payment request. cardToken is the fictitious token of the
// payer's card, required for a card payment and refused for any other.
func NewDraft(merchant MerchantID, method string, amount money.Money, reference, cardToken string) (Draft, error) {
	m, err := ParseMethod(method)
	if err != nil {
		return Draft{}, err
	}
	if err := checkCardToken(m, cardToken); err != nil {
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
	return Draft{merchant: merchant, method: m, amount: amount, reference: reference, cardToken: cardToken}, nil
}

func checkCardToken(m Method, token string) error {
	switch {
	case m != Card && token != "":
		return ErrCardNotAllowed
	case m != Card:
		return nil
	case token == "":
		return ErrCardRequired
	case !ValidCardToken(token):
		return ErrInvalidCardToken
	}
	return nil
}

// ValidCardToken accepts "tok_" and 1 to 60 ASCII letters, digits or underscores.
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

// CardToken is empty unless the method is card.
func (d *Draft) CardToken() string { return d.cardToken }

// Payment is a stored payment. Its status only changes through Transition.
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
