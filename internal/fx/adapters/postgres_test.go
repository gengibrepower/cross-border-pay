package adapters_test

import (
	"os"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/fx/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

func TestLoadQuotesReturnsWhatWasSaved(t *testing.T) {
	pool := pgtest.NewPool(t)
	rate, err := domain.ParseRate(money.USD, money.BRL, "4.9873")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	want := map[domain.QuoteID]domain.Quote{}
	for _, amount := range []int64{298, 2000, 999_999} {
		q, err := domain.NewQuote(money.New(amount, money.USD), rate, domain.DefaultPricing, t0, t0.Add(30*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		id, err := adapters.SaveQuote(t.Context(), pool, &q)
		if err != nil {
			t.Fatal(err)
		}
		want[id] = q
	}

	ids := []domain.QuoteID{{0xff}}
	for id := range want {
		ids = append(ids, id)
	}
	got, err := adapters.LoadQuotes(t.Context(), pool, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("loaded %d quotes, want %d: the unknown ID must be absent", len(got), len(want))
	}
	for id, q := range want {
		if got[id] != q {
			t.Errorf("quote %x = %+v, want %+v", id, got[id], q)
		}
	}
}
