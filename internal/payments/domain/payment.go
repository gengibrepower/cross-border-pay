package domain

import (
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gengibrepower/cross-border-pay/internal/money"
)

var (
	ErrUnknownMethod       = errors.New("payments: unknown payment method")
	ErrUnsupportedMethod   = errors.New("payments: payment method not supported yet")
	ErrUnsupportedCurrency = errors.New("payments: merchants charge in USD")
	ErrInvalidAmount       = errors.New("payments: amount must be positive")
	ErrInvalidReference    = errors.New("payments: reference must have 1 to 100 characters")
	ErrInvalidPaymentID    = errors.New("payments: invalid payment ID")
)

// ChargeCurrency is the only currency merchants charge in; payers pay in BRL.
var ChargeCurrency = money.USD

// MerchantID is payments' own name for the merchant a payment belongs to, so this
// module does not import merchants; cmd/api converts between the two.
type MerchantID [16]byte

type PaymentID [16]byte

var idEncoding = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

// String is the public ID (decision 37): "pay_" and the UUIDv7 in Crockford base32,
// whose alphabet is in ASCII order, so IDs sort by creation time as strings too.
func (id PaymentID) String() string { return "pay_" + idEncoding.EncodeToString(id[:]) }

func ParsePaymentID(s string) (PaymentID, error) {
	encoded, ok := strings.CutPrefix(s, "pay_")
	if !ok || len(encoded) != idEncoding.EncodedLen(len(PaymentID{})) {
		return PaymentID{}, fmt.Errorf("%w: %q", ErrInvalidPaymentID, s)
	}
	b, err := idEncoding.DecodeString(encoded)
	if err != nil {
		return PaymentID{}, fmt.Errorf("%w: %q", ErrInvalidPaymentID, s)
	}
	return PaymentID(b), nil
}

type Method string

const (
	Pix    Method = "pix"
	Boleto Method = "boleto"
	Card   Method = "card"
)

// PixQuoteValidity is decision 44 for PIX.
const PixQuoteValidity = 30 * time.Minute

// ParseMethod accepts only PIX until boleto and card arrive in F8.
func ParseMethod(s string) (Method, error) {
	switch m := Method(s); m {
	case Pix:
		return m, nil
	case Boleto, Card:
		return "", fmt.Errorf("%w: %s", ErrUnsupportedMethod, m)
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
}

func NewDraft(merchant MerchantID, method string, amount money.Money, reference string) (Draft, error) {
	m, err := ParseMethod(method)
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
	return Draft{merchant: merchant, method: m, amount: amount, reference: reference}, nil
}

func (d *Draft) Merchant() MerchantID { return d.merchant }

func (d *Draft) Method() Method { return d.method }

func (d *Draft) Amount() money.Money { return d.amount }

func (d *Draft) Reference() string { return d.reference }

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
