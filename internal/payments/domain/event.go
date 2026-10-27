package domain

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/money"
)

type EventID [16]byte

func (id EventID) String() string { return "evt_" + idEncoding.EncodeToString(id[:]) }

type EventType string

const PaymentPaid EventType = "payment.paid"

// PaymentSnapshot is what an event tells the merchant about a payment right after a
// transition.
type PaymentSnapshot struct {
	ID       PaymentID
	Merchant MerchantID
	Version  int
	Status   Status
	Amount   money.Money
}

// MerchantEvent is an event for the merchant, already serialized: Payload is the body
// of section 4.5, the bytes every delivery sends.
type MerchantEvent struct {
	ID        EventID
	Merchant  MerchantID
	Type      EventType
	Payment   PaymentID
	Payload   []byte
	CreatedAt time.Time
}

type paymentEventJSON struct {
	ID             string           `json:"id"`
	Type           EventType        `json:"type"`
	CreatedAt      time.Time        `json:"created_at"`
	PaymentID      string           `json:"payment_id"`
	PaymentVersion int              `json:"payment_version"`
	Data           paymentEventData `json:"data"`
}

type paymentEventData struct {
	Status Status      `json:"status"`
	Amount money.Money `json:"amount"`
}

// NewPaymentEvent serializes the event once. Time is cut to the microsecond the
// database keeps, so the stored created_at and the one in the payload are equal.
func NewPaymentEvent(id EventID, t EventType, p *PaymentSnapshot, now time.Time) (MerchantEvent, error) {
	at := now.UTC().Truncate(time.Microsecond)
	payload, err := json.Marshal(paymentEventJSON{
		ID:             id.String(),
		Type:           t,
		CreatedAt:      at,
		PaymentID:      p.ID.String(),
		PaymentVersion: p.Version,
		Data:           paymentEventData{Status: p.Status, Amount: p.Amount},
	})
	if err != nil {
		return MerchantEvent{}, fmt.Errorf("payments: event %s: %w", t, err)
	}
	return MerchantEvent{ID: id, Merchant: p.Merchant, Type: t, Payment: p.ID, Payload: payload, CreatedAt: at}, nil
}
