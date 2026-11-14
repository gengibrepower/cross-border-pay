package main

import (
	"crypto/rand"
	"net/http"
	"testing"
)

// Decision 27: a merchant whose payable and reserve together are below -US$ 1,000.00
// takes no new payment, of any method, and the refusal is a 422 stored like any other.
// Another merchant is not affected, and running the settlement, which moves the
// reserve to the payable, does not change where the merchant stands.
func TestMerchantOverTheDebtLimitTakesNoPayment(t *testing.T) {
	c := newContract(t)
	key, other := c.merchantKey(), c.merchantKey()
	s := c.settler()

	a := c.paidPaymentOf(key, 120_000)
	c.toLocalDay(2)
	c.settle(s)
	got := c.settlements()
	if len(got) != 1 || got[0].reserved != got[0].available()/10 {
		t.Fatalf("settlements = %+v, want one with a tenth held", got)
	}
	held := got[0].reserved

	c.refund(key, a, 60_000)
	// -60000 + the hold: still above the limit.
	if rec := c.do(postPayment(t, key, rand.Text(), gameStore), true); rec.Code != http.StatusCreated {
		t.Fatalf("create at %d: status = %d %s, want 201", held-60_000, rec.Code, rec.Body)
	}

	c.refund(key, a, 60_000)
	position := held - 120_000
	if position >= -100_000 {
		t.Fatalf("position %d is not below the limit: the test proves nothing", position)
	}
	for _, body := range []string{gameStore, `{"amount": {"value": 2000, "currency": "USD"}, "method": "card", "reference": "c", "card": {"token": "tok_approved"}}`} {
		ikey := rand.Text()
		rec := c.do(postPayment(t, key, ikey, body), true)
		if rec.Code != http.StatusUnprocessableEntity || problemCode(t, rec) != "debt_limit_exceeded" {
			t.Fatalf("create at %d: status = %d %s, want 422 debt_limit_exceeded", position, rec.Code, rec.Body)
		}
		if again := c.do(postPayment(t, key, ikey, body), true); again.Code != rec.Code || again.Body.String() != rec.Body.String() {
			t.Errorf("retry = %d %s, want the stored refusal", again.Code, again.Body)
		}
	}
	if rec := c.do(postPayment(t, other, rand.Text(), gameStore), true); rec.Code != http.StatusCreated {
		t.Errorf("another merchant: status = %d %s, want 201", rec.Code, rec.Body)
	}

	before := c.count("payments")
	c.toLocalDay(3)
	c.settle(s) // the reserve covers what it can: the position is the same
	c.wantBalance(key, "the 3rd", position, 0, 0)
	if rec := c.do(postPayment(t, key, rand.Text(), gameStore), true); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("create after the reserve covered: status = %d %s, want 422", rec.Code, rec.Body)
	}
	if n := c.count("payments"); n != before {
		t.Errorf("%d payments stored by refused requests, want none", n-before)
	}
}
