// Package brcode encodes and parses PIX BR Codes: EMV merchant-presented QR payloads
// as specified by the Banco Central's Manual de Padrões para Iniciação do Pix (v2.10.0).
package brcode

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gengibrepower/cross-border-pay/internal/money"
)

var (
	ErrMalformed    = errors.New("brcode: malformed payload")
	ErrInvalidCRC   = errors.New("brcode: CRC does not match")
	ErrFieldTooLong = errors.New("brcode: field too long")
	ErrInvalidField = errors.New("brcode: invalid field")
	ErrInvalidTxID  = errors.New("brcode: txid must be at most 25 letters or digits")
)

// GUI identifies the PIX arrangement inside a Merchant Account Information template.
const GUI = "br.gov.bcb.pix"

const (
	idPayloadFormat      = "00"
	idPointOfInitiation  = "01"
	idMerchantAccount    = "26"
	idMerchantCategory   = "52"
	idCurrency           = "53"
	idAmount             = "54"
	idCountry            = "58"
	idMerchantName       = "59"
	idMerchantCity       = "60"
	idAdditionalData     = "62"
	idCRC                = "63"
	subGUI               = "00"
	subKey               = "01"
	subInfo              = "02"
	subURL               = "25"
	subTxID              = "05"
	payloadFormatVersion = "01"
	// Point of initiation 12 marks a code that must not be paid more than once.
	singleUse         = "12"
	reusable          = "11"
	categoryNotStated = "0000"
	currencyBRL       = "986"
	countryBR         = "BR"
	noTxID            = "***"
)

// Limits from section 2.6.1 of the manual (key, info, the 99-character template) and
// from the EMV specification it follows (merchant name and city, the amount field).
const (
	maxTemplate     = 99
	maxKey          = 77
	maxURL          = 77
	maxInfo         = 72
	maxTxID         = 25
	maxMerchantName = 25
	maxMerchantCity = 15
	maxAmount       = 13
)

// Code is a PIX BR Code. A static code carries the receiver's Key, and optionally an
// Amount, a TxID and free text in Info. A dynamic code carries instead the URL of the
// payment's JSON payload, where the amount lives.
type Code struct {
	Key  string
	URL  string
	Info string
	// Amount is in BRL; the zero value means the payer types the amount.
	Amount money.Money
	// TxID identifies the payment for reconciliation; empty means none ("***").
	TxID         string
	MerchantName string
	MerchantCity string
}

func (c *Code) Dynamic() bool { return c.URL != "" }

// Encode returns the payload, CRC included. Fields come in the order of the manual's
// examples, since the order changes the CRC.
func Encode(c *Code) (string, error) {
	if err := c.validate(); err != nil {
		return "", err
	}
	var account strings.Builder
	writeTLV(&account, subGUI, GUI)
	if c.Dynamic() {
		writeTLV(&account, subURL, c.URL)
	} else {
		writeTLV(&account, subKey, c.Key)
		if c.Info != "" {
			writeTLV(&account, subInfo, c.Info)
		}
	}
	if account.Len() > maxTemplate {
		return "", fmt.Errorf("%w: merchant account template has %d characters, at most %d", ErrFieldTooLong, account.Len(), maxTemplate)
	}
	txid := c.TxID
	if txid == "" {
		txid = noTxID
	}
	var additional strings.Builder
	writeTLV(&additional, subTxID, txid)

	var b strings.Builder
	writeTLV(&b, idPayloadFormat, payloadFormatVersion)
	if c.Dynamic() {
		writeTLV(&b, idPointOfInitiation, singleUse)
	}
	writeTLV(&b, idMerchantAccount, account.String())
	writeTLV(&b, idMerchantCategory, categoryNotStated)
	writeTLV(&b, idCurrency, currencyBRL)
	if c.Amount != (money.Money{}) {
		writeTLV(&b, idAmount, formatAmount(c.Amount))
	}
	writeTLV(&b, idCountry, countryBR)
	writeTLV(&b, idMerchantName, c.MerchantName)
	writeTLV(&b, idMerchantCity, c.MerchantCity)
	writeTLV(&b, idAdditionalData, additional.String())
	// The CRC covers its own ID and length, "6304", and nothing after them.
	b.WriteString(idCRC + "04")
	fmt.Fprintf(&b, "%04X", CRC16([]byte(b.String())))
	return b.String(), nil
}

func writeTLV(b *strings.Builder, id, value string) {
	fmt.Fprintf(b, "%s%02d%s", id, len(value), value)
}

func (c *Code) validate() error {
	if (c.Key == "") == (c.URL == "") {
		return fmt.Errorf("%w: exactly one of key (static) and URL (dynamic) is required", ErrInvalidField)
	}
	if c.Dynamic() && c.Info != "" {
		return fmt.Errorf("%w: info is for static codes; a dynamic one carries it in its payload", ErrInvalidField)
	}
	fields := []struct {
		name  string
		value string
		min   int
		max   int
	}{
		{"key", c.Key, 0, maxKey},
		{"URL", c.URL, 0, maxURL},
		{"info", c.Info, 0, maxInfo},
		{"merchant name", c.MerchantName, 1, maxMerchantName},
		{"merchant city", c.MerchantCity, 1, maxMerchantCity},
	}
	for _, f := range fields {
		if !printableASCII(f.value) {
			return fmt.Errorf("%w: %s must be printable ASCII", ErrInvalidField, f.name)
		}
		if len(f.value) > f.max {
			return fmt.Errorf("%w: %s has %d characters, at most %d", ErrFieldTooLong, f.name, len(f.value), f.max)
		}
		if len(f.value) < f.min {
			return fmt.Errorf("%w: %s is required", ErrInvalidField, f.name)
		}
	}
	if err := validateTxID(c.TxID); err != nil {
		return err
	}
	if c.Amount != (money.Money{}) {
		if c.Amount.Currency() != money.BRL || c.Amount.Amount() <= 0 {
			return fmt.Errorf("%w: amount must be a positive BRL amount, got %s", ErrInvalidField, c.Amount)
		}
		if n := len(formatAmount(c.Amount)); n > maxAmount {
			return fmt.Errorf("%w: amount has %d characters, at most %d", ErrFieldTooLong, n, maxAmount)
		}
	}
	return nil
}

// validateTxID applies section 2.6.2: letters and digits only, up to 25.
func validateTxID(s string) error {
	if len(s) > maxTxID {
		return fmt.Errorf("%w: got %d characters", ErrInvalidTxID, len(s))
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return fmt.Errorf("%w: got %q", ErrInvalidTxID, s)
		}
	}
	return nil
}

// ASCII only: EMV counts lengths in characters, and with UTF-8 implementations
// disagree on whether that means bytes or runes.
func printableASCII(s string) bool {
	for i := range len(s) {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// formatAmount writes BRL minor units as the decimal field 54 expects: 10150 is
// "101.50". Integers only, never a float.
func formatAmount(m money.Money) string {
	v := m.Amount()
	return fmt.Sprintf("%d.%02d", v/100, v%100)
}

// parseAmount reads field 54: digits, optionally a point and one or two decimals.
func parseAmount(s string) (money.Money, error) {
	whole, frac, hasPoint := strings.Cut(s, ".")
	if whole == "" || !digits(whole) || (hasPoint && (frac == "" || len(frac) > 2 || !digits(frac))) {
		return money.Money{}, fmt.Errorf("%w: amount %q", ErrInvalidField, s)
	}
	for len(frac) < 2 {
		frac += "0"
	}
	v, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil || v <= 0 {
		return money.Money{}, fmt.Errorf("%w: amount %q", ErrInvalidField, s)
	}
	return money.New(v, money.BRL), nil
}

func digits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
