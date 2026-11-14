// Package boleto encodes and parses the barcode (44 digits) and the linha digitável
// (47 digits) of a boleto de cobrança, as laid out by Febraban's Layout Padrão de
// Arrecadação/Recebimento com Utilização do Código de Barras, with the due date
// factor restarted at 1000 on 22 February 2025.
//
// Barcode, by position (1-based):
//
//	1-3    bank (COMPE code)
//	4      currency, 9 for the real
//	5      check digit of the whole barcode, modulo 11
//	6-9    due date factor
//	10-19  amount in centavos
//	20-44  free field, laid out by each bank
package boleto

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gengibrepower/cross-border-pay/internal/calendar"
	"github.com/gengibrepower/cross-border-pay/internal/money"
)

var (
	ErrMalformed        = errors.New("boleto: malformed code")
	ErrCheckDigit       = errors.New("boleto: check digit does not match")
	ErrInvalidBank      = errors.New("boleto: bank must be 3 digits")
	ErrInvalidFreeField = errors.New("boleto: free field must be 25 digits")
	ErrInvalidAmount    = errors.New("boleto: amount must be from R$ 0,01 to R$ 99.999.999,99")
	ErrInvalidDue       = errors.New("boleto: due date has no factor")
)

const (
	BarcodeLength = 44
	LineLength    = 47
	FreeFieldLen  = 25
	// MaxAmount is ten digits of centavos.
	MaxAmount = 99_999_999_99
	// currencyReal is the only currency code Febraban defines in use.
	currencyReal = '9'
)

// Boleto is what a barcode says. The beneficiary and the "nosso número" live in the
// free field, whose layout is the issuing bank's.
type Boleto struct {
	// Bank is the three-digit COMPE code of the issuing bank.
	Bank      string
	Due       calendar.Date
	Amount    money.Money
	FreeField string
}

// Barcode is the 44 digits the boleto's barcode carries.
func Barcode(b *Boleto) (string, error) {
	switch {
	case len(b.Bank) != 3 || !digits(b.Bank):
		return "", fmt.Errorf("%w: got %q", ErrInvalidBank, b.Bank)
	case len(b.FreeField) != FreeFieldLen || !digits(b.FreeField):
		return "", fmt.Errorf("%w: got %q", ErrInvalidFreeField, b.FreeField)
	case b.Amount.Currency() != money.BRL || b.Amount.Amount() <= 0 || b.Amount.Amount() > MaxAmount:
		return "", fmt.Errorf("%w: got %s", ErrInvalidAmount, b.Amount)
	}
	factor, err := DueFactor(b.Due)
	if err != nil {
		return "", err
	}
	body := fmt.Sprintf("%s%c%04d%010d%s", b.Bank, currencyReal, factor, b.Amount.Amount(), b.FreeField)
	return body[:4] + string(mod11(body)) + body[4:], nil
}

// Line turns a barcode into its linha digitável, 47 digits with no separators:
//
//	field 1  barcode 1-4 and 20-24, and a modulo 10 digit
//	field 2  barcode 25-34, and a modulo 10 digit
//	field 3  barcode 35-44, and a modulo 10 digit
//	field 4  barcode 5, the general check digit
//	field 5  barcode 6-19, factor and amount
func Line(barcode string) (string, error) {
	if err := checkBarcode(barcode); err != nil {
		return "", err
	}
	f1, f2, f3 := barcode[0:4]+barcode[19:24], barcode[24:34], barcode[34:44]
	return f1 + string(mod10(f1)) + f2 + string(mod10(f2)) + f3 + string(mod10(f3)) + barcode[4:19], nil
}

// BarcodeFromLine turns a linha digitável, with or without its dots and spaces, back
// into the barcode, checking each field's digit and then the general one.
func BarcodeFromLine(line string) (string, error) {
	line = strings.NewReplacer(".", "", " ", "").Replace(line)
	if len(line) != LineLength || !digits(line) {
		return "", fmt.Errorf("%w: a linha digitável has %d digits", ErrMalformed, LineLength)
	}
	for n, f := range [][2]int{{0, 9}, {10, 20}, {21, 31}} {
		if got, want := line[f[1]], mod10(line[f[0]:f[1]]); got != want {
			return "", fmt.Errorf("%w: field %d ends in %c, want %c", ErrCheckDigit, n+1, got, want)
		}
	}
	code := line[0:4] + line[32:47] + line[4:9] + line[10:20] + line[21:31]
	if err := checkBarcode(code); err != nil {
		return "", err
	}
	return code, nil
}

// Parse reads a barcode. A factor names a date every 9000 days, so near, such as
// today, picks the one closest to it.
func Parse(barcode string, near calendar.Date) (Boleto, error) {
	if err := checkBarcode(barcode); err != nil {
		return Boleto{}, err
	}
	if barcode[3] != currencyReal {
		return Boleto{}, fmt.Errorf("%w: currency code %c is not the real", ErrMalformed, barcode[3])
	}
	factor, err := strconv.Atoi(barcode[5:9])
	if err != nil || factor < minFactor {
		return Boleto{}, fmt.Errorf("%w: factor %s", ErrInvalidDue, barcode[5:9])
	}
	amount, err := strconv.ParseInt(barcode[9:19], 10, 64)
	if err != nil || amount <= 0 {
		return Boleto{}, fmt.Errorf("%w: %s centavos", ErrInvalidAmount, barcode[9:19])
	}
	return Boleto{
		Bank: barcode[0:3], Due: dueNear(factor, near),
		Amount: money.New(amount, money.BRL), FreeField: barcode[19:44],
	}, nil
}

// FormatLine writes a linha digitável the way a boleto prints it,
// AAAAA.AAAAA BBBBB.BBBBBB CCCCC.CCCCCC D EEEEEEEEEEEEEE. Anything that is not 47
// digits is returned as it came.
func FormatLine(line string) string {
	if len(line) != LineLength || !digits(line) {
		return line
	}
	return line[0:5] + "." + line[5:10] + " " + line[10:15] + "." + line[15:21] + " " +
		line[21:26] + "." + line[26:32] + " " + line[32:33] + " " + line[33:47]
}

func checkBarcode(code string) error {
	if len(code) != BarcodeLength || !digits(code) {
		return fmt.Errorf("%w: a barcode has %d digits", ErrMalformed, BarcodeLength)
	}
	if got, want := code[4], mod11(code[:4]+code[5:]); got != want {
		return fmt.Errorf("%w: general digit is %c, want %c", ErrCheckDigit, got, want)
	}
	return nil
}

func digits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}
