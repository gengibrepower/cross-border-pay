package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

func TestRunStopsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out bytes.Buffer

	if err := run(ctx, func(string) string { return "" }, &out); err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "worker stopped") {
		t.Errorf("log = %s, want a \"worker stopped\" entry", out.String())
	}
}

func TestRunRejectsInvalidConfig(t *testing.T) {
	getenv := func(key string) string {
		if key == "SHUTDOWN_TIMEOUT" {
			return "ten"
		}
		return ""
	}
	if err := run(t.Context(), getenv, io.Discard); err == nil {
		t.Fatal("run() error = nil, want config error")
	}
}
