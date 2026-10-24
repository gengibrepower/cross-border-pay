package main

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"regexp"
	"testing"

	"github.com/gengibrepower/cross-border-pay/internal/merchants/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/merchants/app"
	"github.com/gengibrepower/cross-border-pay/internal/merchants/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

func TestCreateMerchantAndRevokeKey(t *testing.T) {
	pool := pgtest.NewPool(t)
	url := pool.Config().ConnString()
	getenv := func(k string) string {
		if k == "DATABASE_URL" {
			return url
		}
		return ""
	}

	var out bytes.Buffer
	if err := run(t.Context(), []string{"merchant", "create", "-name", "GameStore", "-mode", "test"}, getenv, &out, rand.Reader); err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`api key\s+(sk_test_([a-z2-7]+)_[a-z2-7]+)\n`).FindStringSubmatch(out.String())
	if m == nil {
		t.Fatalf("no key in output:\n%s", out.String())
	}
	key, lookup := m[1], m[2]

	svc := app.NewService(adapters.NewPostgres(pool), rand.Reader)
	p, err := svc.Authenticate(t.Context(), key)
	if err != nil || p.Mode != domain.Test {
		t.Fatalf("Authenticate(printed key) = %+v, %v", p, err)
	}

	if err := run(t.Context(), []string{"key", "revoke", "-lookup", lookup}, getenv, &out, rand.Reader); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(t.Context(), key); !errors.Is(err, app.ErrUnauthorized) {
		t.Errorf("after revoke: error = %v, want %v", err, app.ErrUnauthorized)
	}
}

func TestUsageErrors(t *testing.T) {
	url := pgtest.NewDatabase(t)
	getenv := func(k string) string {
		if k == "DATABASE_URL" {
			return url
		}
		return ""
	}
	for _, args := range [][]string{
		nil,
		{"merchant"},
		{"merchant", "delete"},
		{"merchant", "create", "-mode", "prod"},
		{"merchant", "create", "-color", "red"},
		{"key", "revoke"},
	} {
		if err := run(t.Context(), args, getenv, &bytes.Buffer{}, rand.Reader); !errors.Is(err, errUsage) {
			t.Errorf("run(%q) error = %v, want %v", args, err, errUsage)
		}
	}
}
