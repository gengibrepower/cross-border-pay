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

// BoletoDueDays is decision 44, in calendar days in the calendar's zone.
const BoletoDueDays = 3

// PayerInput's zero value is no payer.
type PayerInput struct {
	Name     string
	Document string
	Email    string
}

// Payer is decision 54. The CPF prints masked, so a Payer can be logged as it is.
type Payer struct {
	Name     string
	Document cpf.CPF
	Email    string
}

func (p *Payer) IsZero() bool { return p.Name == "" && p.Document.IsZero() && p.Email == "" }

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

// BoletoInput is stored so a request that died before calling the bank can still be
// completed the same way.
type BoletoInput struct {
	Payer Payer
	Due   calendar.Date
}

// BoletoTerms: a boleto is received until the end of the first banking day on or
// after its due date (decision 53), and the quote lasts until then (decision 44).
func BoletoTerms(cal *calendar.Calendar, now time.Time) (due calendar.Date, payableUntil time.Time) {
	due = cal.Today(now).AddDays(BoletoDueDays)
	return due, cal.OnOrAfter(due).EndIn(cal.Location())
}
