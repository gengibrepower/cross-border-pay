// Command demo tells the GameStore story of section 4.1 against a running gateway:
// a PIX paid and when it settles, then a card charged back and won. It plays the
// merchant over the public API, and the payer and the acquirer at the simulators.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gengibrepower/cross-border-pay/internal/calendar"
	merchantsadapters "github.com/gengibrepower/cross-border-pay/internal/merchants/adapters"
	merchantsapp "github.com/gengibrepower/cross-border-pay/internal/merchants/app"
	merchants "github.com/gengibrepower/cross-border-pay/internal/merchants/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	payments "github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
	"github.com/gengibrepower/cross-border-pay/internal/providers/cardsim"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
	webhooks "github.com/gengibrepower/cross-border-pay/internal/webhooks/domain"
)

// waitFor bounds every wait, so a broken stack fails the demo instead of hanging it.
const waitFor = time.Minute

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Getenv, os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
}

type demo struct {
	out     io.Writer
	pool    *pgxpool.Pool
	api     string
	key     string
	pix     *pixsim.Client
	card    *cardsim.Client
	secret  string
	webhook chan event
	last    event
}

type event struct {
	Type      string `json:"type"`
	PaymentID string `json:"payment_id"`
	Data      struct {
		ChargebackID string `json:"chargeback_id"`
	} `json:"data"`
}

type payment struct {
	ID     string      `json:"id"`
	Status string      `json:"status"`
	Amount money.Money `json:"amount"`
	Quote  struct {
		PayerAmount money.Money `json:"payer_amount"`
		Fee         money.Money `json:"fee"`
		Net         money.Money `json:"net_amount"`
	} `json:"quote"`
}

func run(ctx context.Context, getenv func(string) string, out io.Writer) error {
	env := func(k, fallback string) string {
		if v := getenv(k); v != "" {
			return v
		}
		return fallback
	}
	d := &demo{out: out, api: env("DEMO_API_URL", "http://localhost:8080"), webhook: make(chan event, 16)}
	var err error
	if d.pix, err = pixsim.NewClient(env("DEMO_PIXSIM_URL", "http://localhost:8090"), nil); err != nil {
		return fmt.Errorf("DEMO_PIXSIM_URL: %w", err)
	}
	if d.card, err = cardsim.NewClient(env("DEMO_CARDSIM_URL", "http://localhost:8091"), nil); err != nil {
		return fmt.Errorf("DEMO_CARDSIM_URL: %w", err)
	}
	for _, health := range []string{d.api + "/readyz", env("DEMO_PIXSIM_URL", "http://localhost:8090") + "/healthz", env("DEMO_CARDSIM_URL", "http://localhost:8091") + "/healthz"} {
		if err := until(ctx, func() bool { return get(ctx, health) == nil }); err != nil {
			return fmt.Errorf("%s never answered: %w", health, err)
		}
	}
	if d.pool, err = postgres.Connect(ctx, getenv("DATABASE_URL")); err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer d.pool.Close()

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", env("DEMO_ADDR", "127.0.0.1:0"))
	if err != nil {
		return fmt.Errorf("DEMO_ADDR: %w", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(d.receive), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	webhookURL, err := reachableAt(ctx, ln.Addr().(*net.TCPAddr), d.api)
	if err != nil {
		return err
	}
	return d.story(ctx, env("DEMO_WEBHOOK_URL", webhookURL))
}

// reachableAt is the demo's own URL as the gateway sees it. Listening on every
// interface, as in compose, it is the address the demo reaches the API from: `docker
// compose run` gives the demo no name the worker could look up.
func reachableAt(ctx context.Context, addr *net.TCPAddr, api string) (string, error) {
	if !addr.IP.IsUnspecified() {
		return "http://" + addr.String(), nil
	}
	u, err := url.Parse(api)
	if err != nil {
		return "", fmt.Errorf("DEMO_API_URL: %w", err)
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "udp", u.Host)
	if err != nil {
		return "", fmt.Errorf("find the demo's address: %w", err)
	}
	defer func() { _ = conn.Close() }()
	local := conn.LocalAddr().(*net.UDPAddr)
	return "http://" + net.JoinHostPort(local.IP.String(), strconv.Itoa(addr.Port)), nil
}

func (d *demo) story(ctx context.Context, webhookURL string) error {
	id, key, err := merchantsapp.NewService(merchantsadapters.NewPostgres(d.pool), rand.Reader).CreateMerchant(ctx, "GameStore", merchants.Test)
	if err != nil {
		return fmt.Errorf("create the GameStore: %w", err)
	}
	d.key = key
	d.say("The GameStore signs up (merchant %s) and gets a test API key.", id)
	var endpoint struct{ Secret string }
	if err := d.call(ctx, http.MethodPut, "/v1/webhook-endpoint", "", map[string]string{"url": webhookURL + "/webhooks"}, &endpoint); err != nil {
		return err
	}
	d.secret = endpoint.Secret
	d.say("It registers %s/webhooks for its events and keeps the signing secret.\n", webhookURL)

	if err := d.pixStory(ctx); err != nil {
		return err
	}
	if err := d.cardStory(ctx); err != nil {
		return err
	}
	d.say("That is the GameStore story: every step above went through the real API, worker and simulators.")
	return nil
}

func (d *demo) pixStory(ctx context.Context) error {
	d.say("1. PIX")
	var p payment
	err := d.call(ctx, http.MethodPost, "/v1/payments", "demo-pix-"+rand.Text(),
		map[string]any{"amount": money.New(2000, money.USD), "method": "pix", "reference": "order-pix"}, &p)
	if err != nil {
		return err
	}
	d.say("   A player buys %s of credits. The gateway quotes it: the payer pays %s by PIX,", p.Amount, p.Quote.PayerAmount)
	d.say("   the GameStore gets %s after a fee of %s.", p.Quote.Net, p.Quote.Fee)
	charge, err := d.providerID(ctx, p.ID)
	if err != nil {
		return err
	}
	if _, err := d.pix.Pay(ctx, charge); err != nil {
		return fmt.Errorf("pay the PIX: %w", err)
	}
	d.say("   The payer pays the BR Code at the PIX simulator.")
	if err := d.expect(ctx, "payment.paid", p.ID); err != nil {
		return err
	}
	d.say("   The simulator tells the gateway, the worker confirms and posts to the ledger,")
	d.say("   and the GameStore receives payment.paid, signed: the signature checks out.")
	if err := d.balance(ctx); err != nil {
		return err
	}
	policy, err := payments.DefaultSettlementPolicy()
	if err != nil {
		return fmt.Errorf("settlement policy: %w", err)
	}
	day := calendar.On(time.Now(), policy.Location).AddDays(policy.Days[payments.Pix])
	d.say("   PIX settles D+%d: the money is available on %s and paid out by that day's settlement.\n", policy.Days[payments.Pix], day)
	return nil
}

func (d *demo) cardStory(ctx context.Context) error {
	d.say("2. Card and chargeback")
	var p payment
	err := d.call(ctx, http.MethodPost, "/v1/payments", "demo-card-"+rand.Text(),
		map[string]any{"amount": money.New(1500, money.USD), "method": "card", "reference": "order-card", "card": map[string]string{"token": "tok_approved"}}, &p)
	if err != nil {
		return err
	}
	if err := d.expect(ctx, "payment.paid", p.ID); err != nil {
		return err
	}
	d.say("   Another player pays %s by card: authorized and captured for %s, status %s.", p.Amount, p.Quote.PayerAmount, p.Status)
	if err := d.balance(ctx); err != nil {
		return err
	}
	authorization, err := d.providerID(ctx, p.ID)
	if err != nil {
		return err
	}
	cb, err := d.card.OpenChargeback(ctx, authorization, cardsim.OpenChargeback{Amount: p.Quote.PayerAmount, Reason: "cardholder does not recognize it"})
	if err != nil {
		return fmt.Errorf("open the chargeback: %w", err)
	}
	if err := d.expect(ctx, "chargeback.opened", p.ID); err != nil {
		return err
	}
	opened := d.last
	d.say("   The cardholder disputes it: the acquirer opens a chargeback and takes the %s back.", cb.Amount)
	if err := d.balance(ctx); err != nil {
		return err
	}
	if err := d.call(ctx, http.MethodPost, "/v1/chargebacks/"+opened.Data.ChargebackID+"/contest", "",
		map[string]string{"evidence": "the player used the credits in match 4521"}, nil); err != nil {
		return err
	}
	d.say("   The GameStore contests with its evidence.")
	if _, err := d.card.ResolveChargeback(ctx, cb.ID, cardsim.Won); err != nil {
		return fmt.Errorf("resolve the chargeback: %w", err)
	}
	if err := d.expect(ctx, "chargeback.won", p.ID); err != nil {
		return err
	}
	d.say("   The issuer decides for the GameStore: chargeback.won, and the money comes back.")
	if err := d.balance(ctx); err != nil {
		return err
	}
	d.say("")
	return nil
}

// providerID is what the payer or the acquirer the demo plays knows of a payment: the
// provider's own ID. The merchant's API never shows it, so the demo reads it where the
// gateway stored it.
func (d *demo) providerID(ctx context.Context, payment string) (string, error) {
	id, err := payments.ParsePaymentID(payment)
	if err != nil {
		return "", fmt.Errorf("payment ID: %w", err)
	}
	var external string
	err = until(ctx, func() bool {
		return d.pool.QueryRow(ctx, `SELECT external_id FROM payment_attempts
			WHERE payment_id = $1 AND external_id IS NOT NULL ORDER BY created_at DESC LIMIT 1`, [16]byte(id)).Scan(&external) == nil
	})
	if err != nil {
		return "", fmt.Errorf("no provider ID for %s: %w", payment, err)
	}
	return external, nil
}

func (d *demo) balance(ctx context.Context) error {
	var b struct{ Available, Pending money.Money }
	if err := d.call(ctx, http.MethodGet, "/v1/balance", "", nil, &b); err != nil {
		return err
	}
	d.say("   GET /v1/balance: %s available, %s pending.", b.Available, b.Pending)
	return nil
}

func (d *demo) receive(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil || webhooks.Verify(d.secret, r.Header.Get(webhooks.SignatureHeader), body, time.Now()) != nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var e event
	if json.Unmarshal(body, &e) == nil {
		d.webhook <- e
	}
}

// expect waits for the signed event of type for the payment, skipping the others: the
// gateway delivers at least once and in no promised order.
func (d *demo) expect(ctx context.Context, typ, payment string) error {
	timeout := time.After(waitFor)
	for {
		select {
		case e := <-d.webhook:
			if e.Type == typ && e.PaymentID == payment {
				d.last = e
				return nil
			}
		case <-timeout:
			return fmt.Errorf("no %s for %s after %v", typ, payment, waitFor)
		case <-ctx.Done():
			return fmt.Errorf("stopped: %w", ctx.Err())
		}
	}
}

func (d *demo) call(ctx context.Context, method, path, idempotencyKey string, body, into any) error {
	var r io.Reader = http.NoBody
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%s %s: %w", method, path, err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.api+path, r)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+d.key)
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, b)
	}
	if into != nil {
		if err := json.Unmarshal(b, into); err != nil {
			return fmt.Errorf("%s %s: %w", method, path, err)
		}
	}
	return nil
}

func (d *demo) say(format string, args ...any) { _, _ = fmt.Fprintf(d.out, format+"\n", args...) }

func get(ctx context.Context, rawURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return fmt.Errorf("GET %s: %w", rawURL, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", rawURL, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	return nil
}

func until(ctx context.Context, ok func() bool) error {
	deadline := time.Now().Add(waitFor)
	for !ok() {
		if time.Now().After(deadline) {
			return fmt.Errorf("gave up after %v", waitFor)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("stopped: %w", ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	return nil
}
