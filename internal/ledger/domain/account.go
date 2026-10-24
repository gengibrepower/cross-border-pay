package domain

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/gengibrepower/cross-border-pay/internal/money"
)

var ErrInvalidAccount = errors.New("ledger: invalid account")

// AccountID is the UUIDv7 the database assigns. Its byte order is creation order,
// which is the order accounts are locked in (decision 10b).
type AccountID [16]byte

func (id AccountID) Compare(other AccountID) int {
	return bytes.Compare(id[:], other[:])
}

func (id AccountID) String() string {
	h := hex.EncodeToString(id[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

type BalanceRule int

const (
	MayGoNegative BalanceRule = iota + 1
	MustStayNonNegative
)

type Account struct {
	id       AccountID
	currency money.Currency
	rule     BalanceRule
}

func NewAccount(id AccountID, currency money.Currency, rule BalanceRule) (Account, error) {
	if currency == (money.Currency{}) {
		return Account{}, fmt.Errorf("%w: %s has no currency", ErrInvalidAccount, id)
	}
	if rule != MayGoNegative && rule != MustStayNonNegative {
		return Account{}, fmt.Errorf("%w: %s has unknown balance rule %d", ErrInvalidAccount, id, rule)
	}
	return Account{id: id, currency: currency, rule: rule}, nil
}

func (a Account) ID() AccountID { return a.id }

func (a Account) Currency() money.Currency { return a.currency }

func (a Account) Rule() BalanceRule { return a.rule }
