package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

func TestRunAppliesMigrations(t *testing.T) {
	url := pgtest.NewDatabase(t)
	getenv := func(key string) string {
		if key == "DATABASE_URL" {
			return url
		}
		return ""
	}
	var out bytes.Buffer

	if err := run(t.Context(), getenv, &out); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if !strings.Contains(out.String(), "migrations applied") {
		t.Errorf("log = %s, want a \"migrations applied\" entry", out.String())
	}
}

func TestRunRequiresDatabaseURL(t *testing.T) {
	if err := run(t.Context(), func(string) string { return "" }, io.Discard); err == nil {
		t.Fatal("run() error = nil, want missing DATABASE_URL")
	}
}
