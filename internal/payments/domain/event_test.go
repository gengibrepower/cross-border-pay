package domain_test

import (
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

// Section 4.5, byte for byte: the bytes stored are the bytes every delivery sends.
func TestNewPaymentEvent(t *testing.T) {
	snapshot := domain.PaymentSnapshot{
		ID: domain.PaymentID{1}, Merchant: domain.MerchantID{2}, Version: 2,
		Status: domain.Paid, Amount: money.New(2000, money.USD),
	}
	at := time.Date(2026, 11, 3, 14, 21, 7, 123456789, time.FixedZone("BRT", -3*60*60))
	e, err := domain.NewPaymentEvent(domain.EventID{3}, domain.PaymentPaid, &snapshot, at)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"evt_0c000000000000000000000000","type":"payment.paid","created_at":"2026-11-03T17:21:07.123456Z",` +
		`"payment_id":"pay_04000000000000000000000000","payment_version":2,` +
		`"data":{"status":"paid","amount":{"value":2000,"currency":"USD"}}}`
	if string(e.Payload) != want {
		t.Errorf("payload\n got %s\nwant %s", e.Payload, want)
	}
	if e.ID != (domain.EventID{3}) || e.Merchant != (domain.MerchantID{2}) || e.Payment != (domain.PaymentID{1}) ||
		e.Type != domain.PaymentPaid || !e.CreatedAt.Equal(time.Date(2026, 11, 3, 17, 21, 7, 123456000, time.UTC)) {
		t.Errorf("event = %+v", e)
	}
}
