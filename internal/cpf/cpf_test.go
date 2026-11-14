package cpf_test

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/gengibrepower/cross-border-pay/internal/cpf"
)

func TestParse(t *testing.T) {
	// 529.982.247-25 is the CPF most validators document their algorithm with.
	for _, s := range []string{"52998224725", "529.982.247-25", " 529.982.247-25 ", "11144477735", "00000000191"} {
		c, err := cpf.Parse(s)
		if err != nil {
			t.Errorf("Parse(%q) error = %v", s, err)
			continue
		}
		if len(c.Digits()) != cpf.Length || strings.ContainsAny(c.Digits(), ".- ") {
			t.Errorf("Parse(%q).Digits() = %q", s, c.Digits())
		}
	}
	for _, s := range []string{"", "5299822472", "529982247250", "52998224726", "52998224715", "5299822472a", "11111111111", "00000000000", "529-982-247.25x"} {
		if _, err := cpf.Parse(s); !errors.Is(err, cpf.ErrInvalid) {
			t.Errorf("Parse(%q) error = %v, want ErrInvalid", s, err)
		}
	}
}

func TestNew(t *testing.T) {
	c, err := cpf.New("529982247")
	if err != nil || c.Digits() != "52998224725" {
		t.Errorf("New(529982247) = %q, %v, want 52998224725", c.Digits(), err)
	}
	if _, err := cpf.New("1234"); !errors.Is(err, cpf.ErrInvalid) {
		t.Errorf("New(1234) error = %v, want ErrInvalid", err)
	}
	rapid.Check(t, func(t *rapid.T) {
		base := rapid.StringMatching(`[0-9]{9}`).Draw(t, "base")
		c, err := cpf.New(base)
		if strings.Count(base, base[:1]) == 9 {
			return // all equal: the check digits repeat it, which Parse refuses
		}
		if err != nil {
			t.Fatalf("New(%s) error = %v", base, err)
		}
		if _, err := cpf.Parse(c.Digits()); err != nil {
			t.Fatalf("Parse(New(%s)) error = %v", base, err)
		}
		// The check digits are a function of the nine before them, so changing one of
		// them is always caught. A typo in the base is not: remainders 0 and 1 both give
		// a 0, so the Receita's modulo 11 lets a few through.
		i := rapid.IntRange(cpf.Length-2, cpf.Length-1).Draw(t, "position")
		d := c.Digits()
		typo := d[:i] + string('0'+(d[i]-'0'+1)%10) + d[i+1:]
		if _, err := cpf.Parse(typo); err == nil {
			t.Fatalf("Parse(%s), a typo of %s, succeeded", typo, d)
		}
	})
}

func TestNeverPrintsWhole(t *testing.T) {
	c, err := cpf.Parse("52998224725")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Masked(); got != "***.982.247-**" {
		t.Errorf("Masked = %q", got)
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("payer", "cpf", c)
	for _, out := range []string{fmt.Sprintf("%v %s", c, c), fmt.Sprintf("%+v", struct{ Payer cpf.CPF }{c}), buf.String()} {
		if strings.Contains(out, "52998224725") || strings.Contains(out, "529") && strings.Contains(out, "25\"") {
			t.Errorf("printed whole: %s", out)
		}
		if !strings.Contains(out, "***.982.247-**") {
			t.Errorf("not masked: %s", out)
		}
	}
	if (cpf.CPF{}).Masked() != "" {
		t.Error("zero CPF masked to something")
	}
}
