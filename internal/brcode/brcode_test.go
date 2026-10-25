package brcode_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/gengibrepower/cross-border-pay/internal/brcode"
	"github.com/gengibrepower/cross-border-pay/internal/money"
)

// The complete examples of the Manual de Padrões para Iniciação do Pix, v2.10.0
// (sections 2.6.3, 2.7 and the recurrence ones), copied character by character.
const (
	manualStatic           = "00020126580014br.gov.bcb.pix0136123e4567-e12b-12d1-a456-4266554400005204000053039865802BR5913Fulano de Tal6008BRASILIA62070503***63041D3D"
	manualDynamic          = "00020101021226700014br.gov.bcb.pix2548pix.example.com/8b3da2f39a4140d1a91abd93113bd4415204000053039865802BR5913Fulano de Tal6008BRASILIA62070503***630464E4"
	manualRecurrence       = "00020126180014br.gov.bcb.pix5204000053039865802BR5913Fulano de Tal6008BRASILIA62070503***80740014br.gov.bcb.pix2552pix.example.com/rec/2353c790eefb11eaadc10242ac1200026304F2DA"
	manualCompositeStatic  = "00020126580014br.gov.bcb.pix0136123e4567-e12b-12d1-a456-4266554400005204000053039865406100.505802BR5913Fulano de Tal6008BRASILIA62070503***80740014br.gov.bcb.pix2552pix.example.com/rec/2353c790eefb11eaadc10242ac12000263042875"
	manualCompositeDynamic = "00020101021226700014br.gov.bcb.pix2548pix.example.com/8b3da2f39a4140d1a91abd93113bd4415204000053039865802BR5913Fulano de Tal6008BRASILIA62070503***80740014br.gov.bcb.pix2552pix.example.com/rec/2353c790eefb11eaadc10242ac1200026304FB42"
)

const fulanoKey = "123e4567-e12b-12d1-a456-426655440000"

func TestCRC16CheckValue(t *testing.T) {
	// The standard check value of CRC-16/CCITT-FALSE.
	if got := brcode.CRC16([]byte("123456789")); got != 0x29B1 {
		t.Errorf("CRC16(123456789) = %04X, want 29B1", got)
	}
}

func TestManualExamples(t *testing.T) {
	fulano := brcode.Code{Key: fulanoKey, MerchantName: "Fulano de Tal", MerchantCity: "BRASILIA"}
	dynamic := brcode.Code{URL: "pix.example.com/8b3da2f39a4140d1a91abd93113bd441", MerchantName: "Fulano de Tal", MerchantCity: "BRASILIA"}
	withAmount := fulano
	withAmount.Amount = money.New(10050, money.BRL)

	tests := []struct {
		name     string
		payload  string
		want     brcode.Code
		reencode bool
	}{
		{"static", manualStatic, fulano, true},
		{"dynamic", manualDynamic, dynamic, true},
		// The recurrence template (80) is skipped, so these do not re-encode to the same string.
		{"composite static", manualCompositeStatic, withAmount, false},
		{"composite dynamic", manualCompositeDynamic, dynamic, false},
	}
	for _, tt := range tests {
		got, err := brcode.Parse(tt.payload)
		if err != nil || got != tt.want {
			t.Errorf("%s: Parse = %+v, %v; want %+v", tt.name, got, err, tt.want)
			continue
		}
		if !tt.reencode {
			continue
		}
		if encoded, err := brcode.Encode(&tt.want); err != nil || encoded != tt.payload {
			t.Errorf("%s: Encode = %q, %v; want the manual's\n%q", tt.name, encoded, err, tt.payload)
		}
	}
}

// A recurrence-only code has a PIX template with no key and no URL: nothing to pay
// here, and recurrence is out of scope.
func TestManualRecurrenceOnlyIsRejected(t *testing.T) {
	if _, err := brcode.Parse(manualRecurrence); !errors.Is(err, brcode.ErrInvalidField) {
		t.Errorf("Parse(recurrence only) error = %v, want %v", err, brcode.ErrInvalidField)
	}
}

// signed appends a correct CRC, so a test can break one thing and keep the CRC valid.
func signed(body string) string {
	body += "6304"
	return fmt.Sprintf("%s%04X", body, brcode.CRC16([]byte(body)))
}

func TestParseRejects(t *testing.T) {
	staticBody := strings.TrimSuffix(manualStatic, "1D3D")
	staticBody = strings.TrimSuffix(staticBody, "6304")
	tests := []struct {
		name    string
		payload string
		want    error
	}{
		{"CRC changed", strings.TrimSuffix(manualStatic, "1D3D") + "1D3E", brcode.ErrInvalidCRC},
		{"one character changed", strings.Replace(manualStatic, "Fulano", "Fulana", 1), brcode.ErrInvalidCRC},
		{"CRC computed without 6304", staticBody + "6304" + fmt.Sprintf("%04X", brcode.CRC16([]byte(staticBody))), brcode.ErrInvalidCRC},
		{"CRC not hexadecimal", strings.TrimSuffix(manualStatic, "1D3D") + "1D3G", brcode.ErrMalformed},
		{"no CRC field", staticBody, brcode.ErrMalformed},
		{"length longer than the rest", signed("000201260400"), brcode.ErrMalformed},
		{"field twice", signed(staticBody + "5802BR"), brcode.ErrMalformed},
		{"no payload format indicator", signed(strings.TrimPrefix(staticBody, "000201")), brcode.ErrMalformed},
		{"no PIX template", signed(strings.Replace(staticBody, "br.gov.bcb.pix", "br.gov.bcb.xyz", 1)), brcode.ErrMalformed},
		{"currency USD", signed(strings.Replace(staticBody, "5303986", "5303840", 1)), brcode.ErrInvalidField},
		{"txid with a hyphen", signed(strings.Replace(staticBody, "62070503***", "62090505ab-cd", 1)), brcode.ErrInvalidTxID},
		{"no txid", signed(strings.Replace(staticBody, "62070503***", "62040100", 1)), brcode.ErrMalformed},
		{"accented city", signed(strings.Replace(staticBody, "6008BRASILIA", "6009SÃOPAULO", 1)), brcode.ErrMalformed},
		{"decimal comma", signed(strings.Replace(staticBody, "5802BR", "5406100,505802BR", 1)), brcode.ErrInvalidField},
	}
	for _, tt := range tests {
		if _, err := brcode.Parse(tt.payload); !errors.Is(err, tt.want) {
			t.Errorf("%s: Parse error = %v, want %v", tt.name, err, tt.want)
		}
	}
}

func TestEncodeRejects(t *testing.T) {
	valid := brcode.Code{Key: fulanoKey, MerchantName: "Fulano de Tal", MerchantCity: "BRASILIA"}
	tests := []struct {
		name   string
		change func(c *brcode.Code)
		want   error
	}{
		{"key and URL", func(c *brcode.Code) { c.URL = "pix.example.com/x" }, brcode.ErrInvalidField},
		{"neither key nor URL", func(c *brcode.Code) { c.Key = "" }, brcode.ErrInvalidField},
		{"info on a dynamic code", func(c *brcode.Code) { c.Key, c.URL, c.Info = "", "pix.example.com/x", "hi" }, brcode.ErrInvalidField},
		{"no merchant name", func(c *brcode.Code) { c.MerchantName = "" }, brcode.ErrInvalidField},
		{"merchant name of 26", func(c *brcode.Code) { c.MerchantName = strings.Repeat("n", 26) }, brcode.ErrFieldTooLong},
		{"merchant city of 16", func(c *brcode.Code) { c.MerchantCity = strings.Repeat("c", 16) }, brcode.ErrFieldTooLong},
		{"accented city", func(c *brcode.Code) { c.MerchantCity = "São Paulo" }, brcode.ErrInvalidField},
		{"key of 78", func(c *brcode.Code) { c.Key = strings.Repeat("k", 78) }, brcode.ErrFieldTooLong},
		// Key and info share the 99 characters of the template (section 2.6.1).
		{"key and info over the template", func(c *brcode.Code) { c.Key, c.Info = strings.Repeat("k", 77), strings.Repeat("i", 10) }, brcode.ErrFieldTooLong},
		{"txid of 26", func(c *brcode.Code) { c.TxID = strings.Repeat("t", 26) }, brcode.ErrInvalidTxID},
		{"txid with a hyphen", func(c *brcode.Code) { c.TxID = "pay-1" }, brcode.ErrInvalidTxID},
		{"amount in USD", func(c *brcode.Code) { c.Amount = money.New(100, money.USD) }, brcode.ErrInvalidField},
		{"negative amount", func(c *brcode.Code) { c.Amount = money.New(-100, money.BRL) }, brcode.ErrInvalidField},
		{"amount over 13 characters", func(c *brcode.Code) { c.Amount = money.New(1_000_000_000_000, money.BRL) }, brcode.ErrFieldTooLong},
	}
	for _, tt := range tests {
		c := valid
		tt.change(&c)
		if _, err := brcode.Encode(&c); !errors.Is(err, tt.want) {
			t.Errorf("%s: Encode error = %v, want %v", tt.name, err, tt.want)
		}
	}
}

func TestAmountField(t *testing.T) {
	base := brcode.Code{Key: fulanoKey, MerchantName: "Fulano de Tal", MerchantCity: "BRASILIA"}
	for _, tt := range []struct {
		cents   int64
		encoded string
	}{
		{10150, "5406101.50"},
		{1, "54040.01"},
		{100, "54041.00"},
		{999_999_999_999, "54139999999999.99"},
	} {
		c := base
		c.Amount = money.New(tt.cents, money.BRL)
		got, err := brcode.Encode(&c)
		if err != nil || !strings.Contains(got, "5303986"+tt.encoded+"5802BR") {
			t.Errorf("Encode(%d cents) = %q, %v; want field %s", tt.cents, got, err, tt.encoded)
		}
	}

	body := strings.TrimSuffix(strings.TrimSuffix(manualStatic, "1D3D"), "6304")
	for field, cents := range map[string]int64{"5406100.50": 10050, "5405100.5": 10050, "5403100": 10000} {
		c, err := brcode.Parse(signed(strings.Replace(body, "5802BR", field+"5802BR", 1)))
		if err != nil || c.Amount != money.New(cents, money.BRL) {
			t.Errorf("Parse(%s) amount = %v, %v; want %d cents", field, c.Amount, err, cents)
		}
	}
	for _, field := range []string{"5405100.", "5403.50", "54051.234", "540101", "5404-100", "54040.00"} {
		if _, err := brcode.Parse(signed(strings.Replace(body, "5802BR", field+"5802BR", 1))); !errors.Is(err, brcode.ErrInvalidField) && !errors.Is(err, brcode.ErrMalformed) {
			t.Errorf("Parse(%s) error = %v, want it rejected", field, err)
		}
	}
}

func printable(maxLen int) *rapid.Generator[string] {
	return rapid.StringMatching(fmt.Sprintf(`[ -~]{1,%d}`, maxLen))
}

func TestEncodeParseRoundTrips(t *testing.T) {
	alnum := rapid.StringMatching(`[A-Za-z0-9]{0,25}`)
	rapid.Check(t, func(t *rapid.T) {
		c := brcode.Code{
			MerchantName: printable(25).Draw(t, "name"),
			MerchantCity: printable(15).Draw(t, "city"),
			TxID:         alnum.Draw(t, "txid"),
		}
		if rapid.Bool().Draw(t, "dynamic") {
			c.URL = printable(77).Draw(t, "url")
		} else {
			c.Key = printable(40).Draw(t, "key")
			if rapid.Bool().Draw(t, "info") {
				c.Info = printable(99-18-4-len(c.Key)-4).Draw(t, "info")
			}
		}
		if rapid.Bool().Draw(t, "amount") {
			c.Amount = money.New(rapid.Int64Range(1, 999_999_999_999).Draw(t, "cents"), money.BRL)
		}

		payload, err := brcode.Encode(&c)
		if err != nil {
			t.Fatalf("Encode(%+v): %v", c, err)
		}
		got, err := brcode.Parse(payload)
		if err != nil || got != c {
			t.Fatalf("Parse(Encode(c)) = %+v, %v; want %+v\npayload %q", got, err, c, payload)
		}
	})
}
