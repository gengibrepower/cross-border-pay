package boleto_test

import (
	"errors"
	"testing"

	"pgregory.net/rapid"

	"github.com/gengibrepower/cross-border-pay/internal/boleto"
	"github.com/gengibrepower/cross-border-pay/internal/calendar"
	"github.com/gengibrepower/cross-border-pay/internal/money"
)

// The example the Banco do Brasil manual and most issuers' guides reprint: R$ 1,00
// due 31 December 2007, factor 3737 on the 1997 base.
const (
	exampleBarcode = "00193373700000001000500940144816060680935031"
	exampleLine    = "00190500954014481606906809350314337370000000100"
	examplePrinted = "00190.50095 40144.816069 06809.350314 3 37370000000100"
)

func date(t testing.TB, s string) calendar.Date {
	t.Helper()
	d, err := calendar.ParseDate(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestPublishedExample(t *testing.T) {
	b := boleto.Boleto{
		Bank: "001", Due: date(t, "2007-12-31"), Amount: money.New(100, money.BRL),
		FreeField: "0500940144816060680935031",
	}
	got, err := boleto.Barcode(&b)
	if err != nil || got != exampleBarcode {
		t.Fatalf("Barcode = %q, %v, want %q", got, err, exampleBarcode)
	}
	line, err := boleto.Line(got)
	if err != nil || line != exampleLine {
		t.Fatalf("Line = %q, %v, want %q", line, err, exampleLine)
	}
	if printed := boleto.FormatLine(line); printed != examplePrinted {
		t.Errorf("FormatLine = %q, want %q", printed, examplePrinted)
	}
	back, err := boleto.BarcodeFromLine(examplePrinted)
	if err != nil || back != exampleBarcode {
		t.Errorf("BarcodeFromLine = %q, %v, want %q", back, err, exampleBarcode)
	}
	parsed, err := boleto.Parse(exampleBarcode, date(t, "2008-01-10"))
	if err != nil || parsed != b {
		t.Errorf("Parse = %+v, %v, want %+v", parsed, err, b)
	}
}

func TestDueFactor(t *testing.T) {
	tests := []struct {
		due  string
		want int
	}{
		{due: "2000-07-03", want: 1000},
		{due: "2007-12-31", want: 3737},
		// The 1997 base ran out on 21 February 2025 and Febraban restarted at 1000.
		{due: "2025-02-21", want: 9999},
		{due: "2025-02-22", want: 1000},
		{due: "2025-02-23", want: 1001},
		{due: "2026-11-17", want: 1633},
		{due: "2049-10-13", want: 9999},
		{due: "2049-10-14", want: 1000},
	}
	for _, tt := range tests {
		got, err := boleto.DueFactor(date(t, tt.due))
		if err != nil || got != tt.want {
			t.Errorf("DueFactor(%s) = %d, %v, want %d", tt.due, got, err, tt.want)
		}
	}
	if _, err := boleto.DueFactor(date(t, "2000-07-02")); !errors.Is(err, boleto.ErrInvalidDue) {
		t.Errorf("DueFactor before the first 1000: error = %v, want ErrInvalidDue", err)
	}
	if _, err := boleto.DueFactor(calendar.Date{}); !errors.Is(err, boleto.ErrInvalidDue) {
		t.Errorf("DueFactor(zero) error = %v, want ErrInvalidDue", err)
	}
}

func TestParseResolvesTheFactorNearToday(t *testing.T) {
	b := boleto.Boleto{Bank: "999", Due: date(t, "2026-11-17"), Amount: money.New(52_634, money.BRL), FreeField: "0000000000000000000000042"}
	code, err := boleto.Barcode(&b)
	if err != nil {
		t.Fatal(err)
	}
	// Factor 1633 was also 2002-03-28 on the old base; today picks the new one.
	for _, near := range []string{"2026-11-14", "2020-01-01", "2034-01-01"} {
		got, err := boleto.Parse(code, date(t, near))
		if err != nil || got.Due != b.Due {
			t.Errorf("Parse near %s: due = %s, %v, want %s", near, got.Due, err, b.Due)
		}
	}
	got, err := boleto.Parse(code, date(t, "2003-01-01"))
	if err != nil || got.Due != date(t, "2002-03-28") {
		t.Errorf("Parse near 2003: due = %s, %v, want 2002-03-28", got.Due, err)
	}
}

func TestBarcodeRefuses(t *testing.T) {
	valid := boleto.Boleto{Bank: "999", Due: date(t, "2026-11-17"), Amount: money.New(100, money.BRL), FreeField: "1234567890123456789012345"}
	tests := []struct {
		name string
		edit func(*boleto.Boleto)
		want error
	}{
		{name: "short bank", edit: func(b *boleto.Boleto) { b.Bank = "99" }, want: boleto.ErrInvalidBank},
		{name: "letters in bank", edit: func(b *boleto.Boleto) { b.Bank = "9a9" }, want: boleto.ErrInvalidBank},
		{name: "short free field", edit: func(b *boleto.Boleto) { b.FreeField = "123" }, want: boleto.ErrInvalidFreeField},
		{name: "letters in free field", edit: func(b *boleto.Boleto) { b.FreeField = "12345678901234567890123x5" }, want: boleto.ErrInvalidFreeField},
		{name: "dollars", edit: func(b *boleto.Boleto) { b.Amount = money.New(100, money.USD) }, want: boleto.ErrInvalidAmount},
		{name: "zero", edit: func(b *boleto.Boleto) { b.Amount = money.New(0, money.BRL) }, want: boleto.ErrInvalidAmount},
		{name: "above ten digits", edit: func(b *boleto.Boleto) { b.Amount = money.New(10_000_000_000, money.BRL) }, want: boleto.ErrInvalidAmount},
		{name: "no due date", edit: func(b *boleto.Boleto) { b.Due = calendar.Date{} }, want: boleto.ErrInvalidDue},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := valid
			tt.edit(&b)
			if _, err := boleto.Barcode(&b); !errors.Is(err, tt.want) {
				t.Errorf("error = %v, want %v", err, tt.want)
			}
		})
	}
	b := valid
	b.Amount = money.New(99_999_999_99, money.BRL)
	if _, err := boleto.Barcode(&b); err != nil {
		t.Errorf("the largest amount: %v", err)
	}
}

func TestMalformedCodes(t *testing.T) {
	for _, code := range []string{"", "0019337370000000100050094014481606068093503", exampleBarcode + "1", "0019337370000000100050094014481606068093503x"} {
		if _, err := boleto.Line(code); !errors.Is(err, boleto.ErrMalformed) {
			t.Errorf("Line(%q) error = %v, want ErrMalformed", code, err)
		}
		if _, err := boleto.Parse(code, date(t, "2008-01-01")); !errors.Is(err, boleto.ErrMalformed) {
			t.Errorf("Parse(%q) error = %v, want ErrMalformed", code, err)
		}
	}
	for _, line := range []string{"", exampleLine[:46], exampleLine + "0", "0019050095401448160690680935031433737000000010x"} {
		if _, err := boleto.BarcodeFromLine(line); !errors.Is(err, boleto.ErrMalformed) {
			t.Errorf("BarcodeFromLine(%q) error = %v, want ErrMalformed", line, err)
		}
	}
	// A barcode in another currency carries no amount in reais.
	other := "00103373700000001000500940144816060680935031"
	if code, err := otherCurrency(other); err == nil {
		if _, err := boleto.Parse(code, date(t, "2008-01-01")); !errors.Is(err, boleto.ErrMalformed) {
			t.Errorf("Parse in currency 0: error = %v, want ErrMalformed", err)
		}
	}
	if FormatLine := boleto.FormatLine("123"); FormatLine != "123" {
		t.Errorf("FormatLine of a short line = %q, want it unchanged", FormatLine)
	}
}

// otherCurrency fixes the general digit of a barcode edited by hand, so only the
// currency is wrong.
func otherCurrency(code string) (string, error) {
	for d := byte('1'); d <= '9'; d++ {
		c := code[:4] + string(d) + code[5:]
		if _, err := boleto.Line(c); err == nil {
			return c, nil
		}
	}
	return "", errors.New("no digit fits")
}

func TestEveryTypoIsCaught(t *testing.T) {
	// Changing any one digit of the line breaks a check digit: module 10 catches
	// every single-digit error in its field, and module 11 those in the amount and
	// factor, which no field digit covers.
	for i := range len(exampleLine) {
		for d := byte('0'); d <= '9'; d++ {
			if exampleLine[i] == d {
				continue
			}
			typo := exampleLine[:i] + string(d) + exampleLine[i+1:]
			if _, err := boleto.BarcodeFromLine(typo); !errors.Is(err, boleto.ErrCheckDigit) {
				t.Fatalf("digit %d changed to %c: error = %v, want ErrCheckDigit", i+1, d, err)
			}
		}
	}
}

func TestRoundTripProperty(t *testing.T) {
	start := date(t, "2025-02-22")
	rapid.Check(t, func(t *rapid.T) {
		b := boleto.Boleto{
			Bank:      rapid.StringMatching(`[0-9]{3}`).Draw(t, "bank"),
			Due:       start.AddDays(rapid.IntRange(0, 8999).Draw(t, "due")),
			Amount:    money.New(rapid.Int64Range(1, 99_999_999_99).Draw(t, "amount"), money.BRL),
			FreeField: rapid.StringMatching(`[0-9]{25}`).Draw(t, "free"),
		}
		code, err := boleto.Barcode(&b)
		if err != nil {
			t.Fatal(err)
		}
		line, err := boleto.Line(code)
		if err != nil || len(line) != boleto.LineLength {
			t.Fatalf("Line(%s) = %q, %v", code, line, err)
		}
		back, err := boleto.BarcodeFromLine(boleto.FormatLine(line))
		if err != nil || back != code {
			t.Fatalf("BarcodeFromLine(Line(%s)) = %q, %v", code, back, err)
		}
		parsed, err := boleto.Parse(code, b.Due.AddDays(rapid.IntRange(-4000, 4000).Draw(t, "today")))
		if err != nil || parsed != b {
			t.Fatalf("Parse = %+v, %v, want %+v", parsed, err, b)
		}
	})
}
