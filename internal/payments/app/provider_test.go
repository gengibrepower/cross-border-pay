package app_test

import (
	"testing"

	"github.com/gengibrepower/cross-border-pay/internal/payments/app"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

func TestResultEvent(t *testing.T) {
	for _, tt := range []struct {
		outcome app.Outcome
		want    domain.AttemptEvent
	}{
		{app.Accepted, domain.Accept},
		{app.Rejected, domain.Reject},
		{app.Succeeded, domain.Succeed},
		{0, domain.Ambiguous},
		{app.Succeeded + 1, domain.Ambiguous},
	} {
		r := app.AttemptResult{Outcome: tt.outcome}
		if got := r.Event(); got != tt.want {
			t.Errorf("Outcome(%d).Event() = %s, want %s", tt.outcome, got, tt.want)
		}
	}
}
