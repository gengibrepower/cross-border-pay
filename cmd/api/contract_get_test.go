package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	payments "github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

func getRequest(t *testing.T, key, path string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost:8080"+path, http.NoBody)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return req
}

func (c *contract) createPayment(key string) (id string, body []byte) {
	c.t.Helper()
	rec := c.do(postPayment(c.t, key, "k", gameStore), true)
	if rec.Code != http.StatusCreated {
		c.t.Fatalf("create: status = %d: %s", rec.Code, rec.Body.String())
	}
	var p struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		c.t.Fatal(err)
	}
	return p.ID, rec.Body.Bytes()
}

func TestGetPaymentReturnsWhatCreateReturned(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id, created := c.createPayment(key)

	rec := c.do(getRequest(t, key, "/v1/payments/"+id), true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != string(created) {
		t.Errorf("GET body differs from the POST body; the stored quote must read back as shown:\n got %s\nwant %s", got, created)
	}
}

func TestGetPaymentNotFound(t *testing.T) {
	c := newContract(t)
	owner, other := c.merchantKey(), c.merchantKey()
	id, _ := c.createPayment(owner)

	tests := []struct {
		name, key, path string
		validRequest    bool
		wantStatus      int
		wantCode        string
	}{
		{"another merchant's payment", other, "/v1/payments/" + id, true, http.StatusNotFound, "payment_not_found"},
		{"unknown ID", owner, "/v1/payments/pay_" + "00000000000000000000000000", true, http.StatusNotFound, "payment_not_found"},
		{"malformed ID", owner, "/v1/payments/pay_nope", false, http.StatusNotFound, "payment_not_found"},
		{"no API key", "", "/v1/payments/" + id, false, http.StatusUnauthorized, "unauthorized"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c.t = t
			rec := c.do(getRequest(t, tt.key, tt.path), tt.validRequest)
			if code := problemCode(t, rec); rec.Code != tt.wantStatus || code != tt.wantCode {
				t.Errorf("got %d %s, want %d %s", rec.Code, code, tt.wantStatus, tt.wantCode)
			}
		})
	}
}

type paymentList struct {
	Data []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

func (c *contract) list(key, query string) paymentList {
	c.t.Helper()
	rec := c.do(getRequest(c.t, key, "/v1/payments"+query), true)
	if rec.Code != http.StatusOK {
		c.t.Fatalf("list%s: status = %d: %s", query, rec.Code, rec.Body.String())
	}
	var l paymentList
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
		c.t.Fatal(err)
	}
	return l
}

// Walking every page sees each payment exactly once, newest first, and none of
// another merchant's.
func TestListPaymentsPagesWithoutGapsOrRepeats(t *testing.T) {
	c := newContract(t)
	key, other := c.merchantKey(), c.merchantKey()
	const n, limit = 7, 3
	var created []string
	for range n {
		id, _ := c.createPayment(key)
		created = append(created, id)
	}
	c.createPayment(other)

	var seen []string
	query := fmt.Sprintf("?limit=%d", limit)
	for pages := 1; ; pages++ {
		page := c.list(key, query)
		for _, p := range page.Data {
			seen = append(seen, p.ID)
		}
		if page.NextCursor == nil {
			if pages != 3 || len(page.Data) != n-2*limit {
				t.Errorf("last page is page %d with %d payments, want page 3 with %d", pages, len(page.Data), n-2*limit)
			}
			break
		}
		if len(page.Data) != limit {
			t.Fatalf("page %d has %d payments and a next_cursor, want %d", pages, len(page.Data), limit)
		}
		query = fmt.Sprintf("?limit=%d&cursor=%s", limit, *page.NextCursor)
	}

	// IDs sort by creation time as strings (decision 37), so newest first is descending.
	for i := 1; i < len(seen); i++ {
		if seen[i-1] <= seen[i] {
			t.Errorf("payments are not strictly newest first: %v", seen)
			break
		}
	}
	slices.Sort(seen)
	slices.Sort(created)
	if !slices.Equal(seen, created) {
		t.Errorf("listed %v, want exactly the merchant's %v", seen, created)
	}

	if all := c.list(key, ""); len(all.Data) != n || all.NextCursor != nil {
		t.Errorf("default page has %d payments and next_cursor %v, want all %d and null", len(all.Data), all.NextCursor, n)
	}
}

func TestListPaymentsFiltersByStatus(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	paid, _ := c.createPayment(key)
	c.createPayment(key)
	// Confirmation arrives in F5; until then the test moves the payment by hand.
	id, err := payments.ParsePaymentID(paid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.pool.Exec(t.Context(), "UPDATE payments SET status = 'paid' WHERE id = $1", pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		t.Fatal(err)
	}

	got := c.list(key, "?status=paid")
	if len(got.Data) != 1 || got.Data[0].ID != paid || got.Data[0].Status != "paid" {
		t.Errorf("?status=paid listed %+v, want only %s", got.Data, paid)
	}
	if got := c.list(key, "?status=pending"); len(got.Data) != 1 || got.Data[0].ID == paid {
		t.Errorf("?status=pending listed %+v, want only the other payment", got.Data)
	}
}

func TestListPaymentsErrors(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	tests := []struct {
		name, query  string
		validRequest bool
		wantCode     string
	}{
		{"limit zero", "?limit=0", false, "invalid_limit"},
		{"limit above 100", "?limit=101", false, "invalid_limit"},
		{"limit not a number", "?limit=ten", false, "invalid_limit"},
		{"unknown status", "?status=lost", false, "invalid_status"},
		{"cursor not base64", "?cursor=***", true, "invalid_cursor"},
		{"cursor of the wrong length", "?cursor=" + base64.RawURLEncoding.EncodeToString([]byte("short")), true, "invalid_cursor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c.t = t
			rec := c.do(getRequest(t, key, "/v1/payments"+tt.query), tt.validRequest)
			if code := problemCode(t, rec); rec.Code != http.StatusBadRequest || code != tt.wantCode {
				t.Errorf("got %d %s, want 400 %s", rec.Code, code, tt.wantCode)
			}
		})
	}
}

func problemCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var p struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p.Code
}
