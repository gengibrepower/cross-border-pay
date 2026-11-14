package domain

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gengibrepower/cross-border-pay/internal/calendar"
	"github.com/gengibrepower/cross-border-pay/internal/cpf"
)

var (
	// ErrPayerRequired is a boleto without its payer, and ErrPayerNotAllowed a payer on
	// a payment of another method: only a boleto is registered to someone.
	ErrPayerRequired    = errors.New("payments: a boleto payment needs a payer")
	ErrPayerNotAllowed  = errors.New("payments: only a boleto payment takes a payer")
	ErrInvalidPayerName = errors.New("payments: payer name must have 1 to 100 characters")
	ErrInvalidPayerCPF  = errors.New("payments: payer document must be a valid CPF")
	ErrInvalidEmail     = errors.New("payments: payer email must be a valid address of at most 254 characters")
	ErrBarcodeMismatch  = errors.New("payments: provider changed the attempt's barcode")
)

const (
	maxPayerName = 100
	maxEmail     = 254
)

// BoletoDueDays is decision 44: a boleto is due three calendar days after it is
// issued, in the calendar's own zone.
const BoletoDueDays = 3

// PayerInput is the payer as a merchant sends it, before validation. The zero value is
// no payer.
type PayerInput struct {
	Name     string
	Document string
	Email    string
}

// Payer is who a boleto is registered to (decision 54): a name, a CPF and, optionally,
// an e-mail. The CPF prints masked, so a Payer can be logged as it is.
type Payer struct {
	Name     string
	Document cpf.CPF
	Email    string
}

func (p *Payer) IsZero() bool { return p.Name == "" && p.Document.IsZero() && p.Email == "" }

// NewPayer validates a payer. The name is trimmed, and the CPF is kept bare.
func NewPayer(in PayerInput) (Payer, error) {
	name := strings.TrimSpace(in.Name)
	if n := utf8.RuneCountInString(name); n < 1 || n > maxPayerName {
		return Payer{}, fmt.Errorf("%w: got %d", ErrInvalidPayerName, n)
	}
	doc, err := cpf.Parse(in.Document)
	if err != nil {
		return Payer{}, fmt.Errorf("%w: %w", ErrInvalidPayerCPF, err)
	}
	email := strings.TrimSpace(in.Email)
	if email != "" {
		addr, err := mail.ParseAddress(email)
		if err != nil || addr.Address != email || len(email) > maxEmail {
			return Payer{}, ErrInvalidEmail
		}
	}
	return Payer{Name: name, Document: doc, Email: email}, nil
}

// BoletoInput is what a boleto attempt asks the bank to register, stored with it so a
// request that died before calling the bank can still be completed the same way.
type BoletoInput struct {
	Payer Payer
	Due   calendar.Date
}

// BoletoTerms are the due date of a boleto issued at now and the instant it stops
// being received: the end of the first banking day on or after the due date
// (decision 53). The quote lasts until then (decision 44).
func BoletoTerms(cal *calendar.Calendar, now time.Time) (due calendar.Date, payableUntil time.Time) {
	due = cal.Today(now).AddDays(BoletoDueDays)
	return due, cal.OnOrAfter(due).EndIn(cal.Location())
}
