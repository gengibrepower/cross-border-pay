package domain

import (
	"errors"
	"fmt"

	"github.com/gengibrepower/cross-border-pay/internal/platform/publicid"
)

var ErrInvalidRefundID = errors.New("payments: invalid refund ID")

type RefundID [16]byte

func (id RefundID) String() string { return publicid.Encode("ref_", id) }

func ParseRefundID(s string) (RefundID, error) {
	id, ok := publicid.Parse("ref_", s)
	if !ok {
		return RefundID{}, fmt.Errorf("%w: %q", ErrInvalidRefundID, s)
	}
	return id, nil
}
