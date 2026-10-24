package money

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

var ErrInvalidAmount = errors.New("money: value must be an integer in minor units")

type moneyJSON struct {
	Value    int64    `json:"value"`
	Currency Currency `json:"currency"`
}

func (m Money) MarshalJSON() ([]byte, error) {
	data, err := json.Marshal(moneyJSON{Value: m.amount, Currency: m.currency})
	if err != nil {
		return nil, fmt.Errorf("money: marshal: %w", err)
	}
	return data, nil
}

// UnmarshalJSON parses "value" from its raw bytes so that 20.5, 2e3 and "2000" are
// rejected instead of being coerced into an integer.
func (m *Money) UnmarshalJSON(data []byte) error {
	var raw struct {
		Value    json.RawMessage `json:"value"`
		Currency Currency        `json:"currency"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("money: unmarshal: %w", err)
	}

	amount, err := strconv.ParseInt(string(raw.Value), 10, 64)
	if err != nil {
		return fmt.Errorf("%w: got %s", ErrInvalidAmount, raw.Value)
	}
	if raw.Currency == (Currency{}) {
		return fmt.Errorf("%w: currency is required", ErrUnknownCurrency)
	}

	*m = New(amount, raw.Currency)
	return nil
}
