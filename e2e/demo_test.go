package e2e

import (
	"strings"
	"testing"
)

// The demo of docs/f11.md against the real binaries: it fails, and so does this test,
// as soon as a step it shows stops working.
func TestDemoTellsTheGameStoreStory(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	out := run(t, "demo", map[string]string{
		"DATABASE_URL": s.dbURL, "DEMO_API_URL": s.api, "DEMO_PIXSIM_URL": s.pixsim, "DEMO_CARDSIM_URL": s.cardsim,
	})
	for _, want := range []string{"101.50 BRL", "payment.paid, signed", "chargeback.won", "PIX settles D+1"} {
		if !strings.Contains(out, want) {
			t.Errorf("the demo did not tell %q:\n%s", want, out)
		}
	}
	t.Log(out)
	s.stop()
}
