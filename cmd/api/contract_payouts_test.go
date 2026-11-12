package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"
)

type moneyBody struct {
	Value    int64  `json:"value"`
	Currency string `json:"currency"`
}

type balanceBody struct {
	Available moneyBody `json:"available"`
	Pending   moneyBody `json:"pending"`
	Reserved  moneyBody `json:"reserved"`
}

func (c *contract) getBalance(key string) balanceBody {
	c.t.Helper()
	rec := c.do(getRequest(c.t, key, "/v1/balance"), true)
	if rec.Code != http.StatusOK {
		c.t.Fatalf("balance: status = %d: %s", rec.Code, rec.Body.String())
	}
	var b balanceBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		c.t.Fatal(err)
	}
	for _, m := range []moneyBody{b.Available, b.Pending, b.Reserved} {
		if m.Currency != "USD" {
			c.t.Fatalf("balance %+v is not all in USD", b)
		}
	}
	return b
}

// wantBalance checks the merchant's balance and that it splits the payable, all of it:
// what the platform holds is either available or pending.
func (c *contract) wantBalance(key, when string, available, pending int64) {
	c.t.Helper()
	b := c.getBalance(key)
	if b.Available.Value != available || b.Pending.Value != pending || b.Reserved.Value != 0 {
		c.t.Errorf("%s: balance = %d available, %d pending, %d reserved; want %d, %d, 0",
			when, b.Available.Value, b.Pending.Value, b.Reserved.Value, available, pending)
	}
	if payable := c.balance("merchant_payable:"); b.Available.Value+b.Pending.Value != payable {
		c.t.Errorf("%s: available + pending = %d, but the payable holds %d",
			when, b.Available.Value+b.Pending.Value, payable)
	}
}

// The balance follows what the settlement job pays: a PIX is pending on its local day,
// even once the date in UTC has turned, and available from the next, a refund comes out of the payment's pending net or, once it
// is settled, out of what is available, which may go negative (decision 27).
func TestBalanceSplitsThePayableAsTheSettlementDoes(t *testing.T) {
	c := newContract(t)
	key, other := c.merchantKey(), c.merchantKey()
	s := c.settler()
	c.wantBalance(key, "before any payment", 0, 0)

	a := c.paidPayment(key)
	c.wantBalance(key, "A paid on the 1st", 0, 1940)
	c.refund(key, a, 500)
	c.wantBalance(key, "A refunded 500", 0, 1440)
	c.clock.Advance(14*time.Hour + 30*time.Minute) // 23:30 local on the 1st, the 2nd in UTC
	c.wantBalance(key, "late on the 1st", 0, 1440)

	c.toLocalDay(2)
	c.wantBalance(key, "the 2nd, before the run", 1440, 0)
	c.paidPayment(key) // B
	c.wantBalance(key, "B paid on the 2nd", 1440, 1940)
	c.settle(s)
	c.wantBalance(key, "after the run of the 2nd", 0, 1940)

	c.refund(key, a, 1500) // A is settled: it comes out of what is available
	c.wantBalance(key, "settled A refunded 1500", -1500, 1940)
	c.toLocalDay(3)
	c.wantBalance(key, "the 3rd", 440, 0)

	if got := c.getBalance(other); got.Available.Value != 0 || got.Pending.Value != 0 {
		t.Errorf("another merchant's balance = %+v, want zero", got)
	}
}

type settlementBody struct {
	ID         string    `json:"id"`
	CutoffDate string    `json:"cutoff_date"`
	Amount     moneyBody `json:"amount"`
	Payments   []string  `json:"payments"`
	CreatedAt  time.Time `json:"created_at"`
}

type settlementList struct {
	Data       []settlementBody `json:"data"`
	NextCursor *string          `json:"next_cursor"`
}

func (c *contract) listSettlements(key, query string) settlementList {
	c.t.Helper()
	rec := c.do(getRequest(c.t, key, "/v1/settlements"+query), true)
	if rec.Code != http.StatusOK {
		c.t.Fatalf("settlements%s: status = %d: %s", query, rec.Code, rec.Body.String())
	}
	var l settlementList
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
		c.t.Fatal(err)
	}
	return l
}

// Walking every page sees each settlement once, newest cutoff first, with the same
// amount and payments as stored, and none of another merchant's, settled in the
// same runs.
func TestSettlementsListPagesWithTheirPayments(t *testing.T) {
	c := newContract(t)
	key, other := c.merchantKey(), c.merchantKey()
	s := c.settler()
	var otherID string
	for day := 1; day <= 5; day++ {
		if day > 1 {
			c.toLocalDay(day)
			c.settle(s)
		}
		for range day % 3 {
			c.paidPayment(key)
		}
		if day == 3 { // settled on the 4th, a cutoff with nothing for key
			otherID = c.paidPayment(other)
		}
	}
	var stored []storedSettlement // key's, oldest first
	for _, st := range c.settlements() {
		if !slices.Contains(st.payments, otherID) {
			stored = append(stored, st)
		}
	}
	if len(stored) != 3 {
		t.Fatalf("%d settlements stored for the merchant, want 3: on the 2nd, 3rd and 5th (nothing was paid on the 3rd)", len(stored))
	}

	var walked []settlementBody
	query := "?limit=1"
	for pages := 0; ; pages++ {
		if pages > len(stored) { // a cursor that does not move would page forever
			t.Fatalf("still paging after %d pages of 1 for %d settlements", pages, len(stored))
		}
		page := c.listSettlements(key, query)
		if len(page.Data) > 1 {
			t.Fatalf("page of %d with limit=1", len(page.Data))
		}
		walked = append(walked, page.Data...)
		if page.NextCursor == nil {
			break
		}
		query = "?limit=1&cursor=" + *page.NextCursor
	}
	if len(walked) != len(stored) {
		t.Fatalf("walked %d settlements, want %d", len(walked), len(stored))
	}
	for i, got := range walked {
		want := stored[len(stored)-1-i] // stored is oldest first
		if got.CutoffDate != want.cutoff.Format(time.DateOnly) || got.Amount.Value != want.amount ||
			got.Amount.Currency != "USD" || !slices.Equal(sorted(got.Payments...), want.payments) {
			t.Errorf("settlement %d = %+v, want cutoff %s, %d with %v",
				i, got, want.cutoff.Format(time.DateOnly), want.amount, want.payments)
		}
	}
	if all := c.listSettlements(key, ""); len(all.Data) != len(stored) || all.NextCursor != nil {
		t.Errorf("default page = %d settlements, next %v; want all %d and no cursor", len(all.Data), all.NextCursor, len(stored))
	}
	if l := c.listSettlements(other, ""); len(l.Data) != 1 || l.Data[0].CutoffDate != "2026-11-04" ||
		!slices.Equal(l.Data[0].Payments, []string{otherID}) || l.NextCursor != nil {
		t.Errorf("another merchant's settlements = %+v, want only its own on the 4th with %s", l, otherID)
	}
}

func TestSettlementsListErrors(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	tests := []struct {
		name, query  string
		validRequest bool
		wantCode     string
	}{
		{"limit zero", "?limit=0", false, "invalid_limit"},
		{"limit above 100", "?limit=101", false, "invalid_limit"},
		{"limit not a number", "?limit=ten", false, "invalid_limit"},
		{"cursor not base64", "?cursor=***", true, "invalid_cursor"},
		{"cursor not a date", "?cursor=" + base64.RawURLEncoding.EncodeToString([]byte("yesterday")), true, "invalid_cursor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c.t = t
			rec := c.do(getRequest(t, key, "/v1/settlements"+tt.query), tt.validRequest)
			if code := problemCode(t, rec); rec.Code != http.StatusBadRequest || code != tt.wantCode {
				t.Errorf("got %d %s, want 400 %s", rec.Code, code, tt.wantCode)
			}
		})
	}
}

func TestPayoutsRequireAnAPIKey(t *testing.T) {
	c := newContract(t)
	for _, path := range []string{"/v1/balance", "/v1/settlements"} {
		t.Run(path, func(t *testing.T) {
			c.t = t
			rec := c.do(getRequest(t, "", path), false)
			if code := problemCode(t, rec); rec.Code != http.StatusUnauthorized {
				t.Errorf("got %d %s, want 401", rec.Code, code)
			}
		})
	}
}
