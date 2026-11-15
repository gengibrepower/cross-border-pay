package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/calendar"
)

// chaosOps is the short script CI runs; the nightly job sets CHAOS_OPS to run a long one.
const chaosOps = 24

// plantedForeignKey is a charge paid at the PIX simulator that the gateway never asked
// for: the one divergence the reconciliation must find.
const plantedForeignKey = "chaos-foreign-charge"

// chaos plays a merchant and its payers against the whole stack, one operation at a
// time: the faults are consumed in a fixed order, so the seed decides the whole story.
type chaos struct {
	s   *stack
	key string
	rng *rand.Rand
}

func runChaos(t *testing.T, seed uint64, ops int) string {
	t.Helper()
	s := newSeededStack(t, strconv.FormatUint(seed, 10))
	c := &chaos{s: s, key: s.createMerchant("Chaos Store"), rng: rand.New(rand.NewPCG(seed, seed))} //nolint:gosec // G404: a test's reproducible script, not a secret.
	c.plantForeignCharge()
	for i := range ops {
		c.step(fmt.Sprintf("chaos-%03d", i))
	}
	c.settle()
	c.reconcile()
	s.ledger()
	if t.Failed() {
		t.FailNow()
	}
	state := c.state()
	s.stop()
	return state
}

// fault schedules, one time in three, a fault drawn by the seed on the op the next call
// reaches. fail_before is left out: what the provider never got is only given up after
// NotFoundGrace, a minute per operation, and the resolver's tests already cover it.
func (c *chaos) fault(sim, op string) {
	c.s.t.Helper()
	var f map[string]any
	switch c.rng.IntN(6) {
	case 0:
		f = map[string]any{"fault": "timeout_after"}
	case 1:
		f = map[string]any{"fault": "delay", "delay_ms": 200}
	default:
		return
	}
	f["op"], f["count"] = op, 1
	if status, _, body := c.s.do(http.MethodPost, sim+"/control/faults", "", f); status != http.StatusNoContent {
		c.s.t.Fatalf("fault %v at %s: %d %s", f, sim, status, body)
	}
}

func (c *chaos) step(ref string) {
	c.s.t.Helper()
	body := map[string]any{"amount": map[string]any{"value": 1000 + c.rng.IntN(9000), "currency": "USD"}, "reference": ref}
	var sim, createOp string
	manual := false
	switch c.rng.IntN(3) {
	case 0:
		body["method"], sim, createOp = "pix", c.s.pixsim, "create"
	case 1:
		token := "tok_approved"
		if c.rng.IntN(5) == 0 {
			token = "tok_declined"
		}
		manual = c.rng.IntN(3) == 0
		capture := "automatic"
		if manual {
			capture = "manual"
		}
		body["method"], body["card"], sim, createOp = "card", map[string]any{"token": token, "capture_method": capture}, c.s.cardsim, "authorize"
	default:
		body["method"], body["payer"], sim, createOp = "boleto", map[string]any{"name": "Maria Silva", "document": "529.982.247-25", "email": "maria@example.com"}, c.s.boleto, "create"
	}
	c.fault(sim, createOp)
	status, _, out := c.s.do(http.MethodPost, c.s.api+"/v1/payments", c.key, body, "Idempotency-Key", ref)
	var p payment
	if status >= 500 || json.Unmarshal(out, &p) != nil || p.ID == "" {
		// A provider that failed before answering leaves nothing to follow.
		return
	}
	attempt, external := c.attempt(ref)
	switch {
	case attempt != "accepted" && attempt != "succeeded":
		return
	case body["method"] == "pix":
		c.payAt(c.s.pixsim+"/control/charges/"+external+"/pay", ref)
	case body["method"] == "boleto":
		c.payAt(c.s.boleto+"/control/boletos/"+external+"/pay", ref)
	case manual:
		c.fault(sim, "capture")
		c.s.do(http.MethodPost, c.s.api+"/v1/payments/"+p.ID+"/capture", c.key, nil)
		c.attempt(ref)
	}
	p = c.waitPayment(p.ID, ref)
	if p.Status != "paid" {
		return
	}
	switch c.rng.IntN(6) {
	case 0, 1:
		c.fault(sim, "refund")
		c.s.do(http.MethodPost, c.s.api+"/v1/payments/"+p.ID+"/refunds", c.key,
			map[string]any{"amount": map[string]any{"value": p.Amount.Value / 2, "currency": "USD"}}, "Idempotency-Key", ref+"-refund")
		c.eventually(ref+" refund resolved", `SELECT count(*) = 0 FROM refunds AS r JOIN payments AS p ON p.id = r.payment_id
			WHERE p.reference = $1 AND r.status IN ('created', 'sent', 'unknown')`, ref)
	case 2:
		if body["method"] != "card" {
			return
		}
		cb := map[string]any{"amount": map[string]any{"value": p.Quote.PayerAmount.Value, "currency": "BRL"}, "reason": "fraud"}
		if status, _, out := c.s.do(http.MethodPost, c.s.cardsim+"/control/authorizations/"+external+"/chargebacks", "", cb); status >= 300 {
			c.s.t.Fatalf("open chargeback on %s: %d %s", ref, status, out)
		}
		c.eventually(ref+" chargeback recorded", `SELECT count(*) = 1 FROM chargebacks AS c JOIN payments AS p ON p.id = c.payment_id
			WHERE p.reference = $1`, ref)
	}
}

// attempt waits for the payment's latest attempt to leave the states a provider call
// can still change, as the resolver makes it do.
func (c *chaos) attempt(ref string) (status, external string) {
	c.s.t.Helper()
	eventually(c.s.t, 20*time.Second, ref+" attempt settled", func() bool {
		var ext *string
		err := c.s.pool.QueryRow(c.s.t.Context(), `SELECT a.status, a.external_id FROM payment_attempts AS a
			JOIN payments AS p ON p.id = a.payment_id WHERE p.reference = $1 ORDER BY a.created_at DESC LIMIT 1`, ref).Scan(&status, &ext)
		if err != nil {
			return false
		}
		if ext != nil {
			external = *ext
		}
		return status != "created" && status != "sent" && status != "unknown"
	})
	return status, external
}

func (c *chaos) payAt(url, ref string) {
	c.s.t.Helper()
	if status, _, body := c.s.do(http.MethodPost, url, "", nil); status != http.StatusOK {
		c.s.t.Fatalf("pay %s: %d %s", ref, status, body)
	}
}

// waitPayment waits for what the provider's event brings, if one is coming.
func (c *chaos) waitPayment(id, ref string) payment {
	c.s.t.Helper()
	c.eventually(ref+" events processed", `SELECT count(*) = 0 FROM inbox_events WHERE status = 'pending'`)
	if status, _ := c.attempt(ref); status != "succeeded" {
		p, _ := c.s.getPayment(c.key, id)
		return p
	}
	var p payment
	eventually(c.s.t, 20*time.Second, ref+" paid", func() bool {
		p, _ = c.s.getPayment(c.key, id)
		return p.Status == "paid"
	})
	return p
}

func (c *chaos) eventually(what, query string, args ...any) {
	c.s.t.Helper()
	eventually(c.s.t, 20*time.Second, what, func() bool {
		var ok bool
		return c.s.pool.QueryRow(c.s.t.Context(), query, args...).Scan(&ok) == nil && ok
	})
}

func (c *chaos) plantForeignCharge() {
	c.s.t.Helper()
	status, _, body := c.s.do(http.MethodPost, c.s.pixsim+"/v1/charges", "",
		map[string]any{"amount": map[string]any{"value": 5000, "currency": "BRL"}, "expires_at": time.Now().Add(time.Hour).UTC()},
		"Idempotency-Key", plantedForeignKey)
	var ch struct{ ID string }
	if status != http.StatusCreated || json.Unmarshal(body, &ch) != nil {
		c.s.t.Fatalf("planted charge: %d %s", status, body)
	}
	c.payAt(c.s.pixsim+"/control/charges/"+ch.ID+"/pay", plantedForeignKey)
}

// settle waits until nothing is left for the worker to resolve.
func (c *chaos) settle() {
	c.s.t.Helper()
	c.eventually("the stack settled", `SELECT
		(SELECT count(*) FROM payment_attempts WHERE status IN ('created', 'sent', 'unknown')) +
		(SELECT count(*) FROM refunds WHERE status IN ('created', 'sent', 'unknown')) +
		(SELECT count(*) FROM inbox_events WHERE status = 'pending') = 0`)
}

// reconcile runs today's reconciliation as an operator would, and wants only the
// planted charge found: every other real on the statements is once in the ledger.
// shortcut: a script that crosses midnight in São Paulo splits its movements over two
// days; run it again if it ever does.
func (c *chaos) reconcile() {
	c.s.t.Helper()
	cal, err := calendar.Brazil()
	if err != nil {
		c.s.t.Fatal(err)
	}
	out := run(c.s.t, "worker", c.s.worker, "reconcile", "-day", cal.Today(time.Now()).String())
	got := map[string][3]int{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		var l struct {
			Msg, Provider                string
			Divergences, Settled, Opened int
		}
		if json.Unmarshal(sc.Bytes(), &l) == nil && l.Msg == "provider day reconciled" {
			got[l.Provider] = [3]int{l.Divergences, l.Settled, l.Opened}
		}
	}
	want := map[string][3]int{"pixsim": {1, 1, 0}, "cardsim": {}, "boletosim": {}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		c.s.t.Errorf("reconciliation [divergences settled opened] = %v, want %v\n%s", got, want, out)
	}
}

var generatedIDs = regexp.MustCompile(`[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}`)

// state is what the script left, without the IDs and times the gateway generated.
func (c *chaos) state() string {
	c.s.t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT p.reference, p.method, p.status, p.amount, string_agg(a.status, ',' ORDER BY a.created_at)
		 FROM payments AS p JOIN payment_attempts AS a ON a.payment_id = p.id GROUP BY p.id ORDER BY p.reference`,
		`SELECT p.reference, r.status, r.amount, r.payer_amount FROM refunds AS r JOIN payments AS p ON p.id = r.payment_id ORDER BY p.reference`,
		`SELECT p.reference, c.status, c.amount FROM chargebacks AS c JOIN payments AS p ON p.id = c.payment_id ORDER BY p.reference`,
		`SELECT kind, provider, amount, currency FROM review_cases ORDER BY kind, provider, amount`,
		`SELECT l.code, sum(e.amount)::bigint FROM ledger_entries AS e JOIN ledger_accounts AS l ON l.id = e.account_id GROUP BY l.code ORDER BY l.code`,
	} {
		rows, err := c.s.pool.Query(c.s.t.Context(), q)
		if err != nil {
			c.s.t.Fatalf("%s: %v", q, err)
		}
		for rows.Next() {
			values, err := rows.Values()
			if err != nil {
				c.s.t.Fatal(err)
			}
			fmt.Fprintln(&b, values...)
		}
		rows.Close()
		b.WriteString("--\n")
	}
	return generatedIDs.ReplaceAllString(b.String(), "<id>")
}

func chaosLength(t *testing.T) int {
	t.Helper()
	v := os.Getenv("CHAOS_OPS")
	if v == "" {
		return chaosOps
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		t.Fatalf("CHAOS_OPS = %q, want a positive number", v)
	}
	return n
}

// Decision 7: with faults the seed draws on every call the script makes, the books
// still sum to zero and the reconciliation finds only what was planted. Cited test 5:
// the same seed tells the same story, to the last state; another seed, another one.
func TestChaosSameSeedSameStory(t *testing.T) {
	t.Parallel()
	ops := chaosLength(t)
	seeds := []uint64{42, 42, 43}
	states := make([]string, len(seeds))
	// Each run is sequential inside its own stack, so running the stacks side by side
	// changes no story.
	t.Run("runs", func(t *testing.T) {
		for i, seed := range seeds {
			t.Run(fmt.Sprint(i, "-seed-", seed), func(t *testing.T) {
				t.Parallel()
				states[i] = runChaos(t, seed, ops)
			})
		}
	})
	if t.Failed() {
		return
	}
	if states[1] != states[0] {
		t.Errorf("seed 42 twice left different states:\n%s\nvs\n%s", states[0], states[1])
	}
	if states[2] == states[0] {
		t.Errorf("seeds 42 and 43 left the same state:\n%s", states[0])
	}
}
