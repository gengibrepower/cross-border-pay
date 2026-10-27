package config

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name string
		vars map[string]string
		want Config
	}{
		{
			name: "defaults",
			vars: nil,
			want: Config{HTTPAddr: ":8080", LogLevel: slog.LevelInfo, ShutdownTimeout: 10 * time.Second, USDBRLRate: "5.0000", PixSimURL: "http://localhost:8090", ProviderTimeout: 5 * time.Second},
		},
		{
			name: "all set",
			vars: map[string]string{
				"HTTP_ADDR":             "127.0.0.1:9000",
				"LOG_LEVEL":             "debug",
				"SHUTDOWN_TIMEOUT":      "30s",
				"DATABASE_URL":          "postgres://localhost/cbp",
				"FX_USD_BRL_RATE":       "5.4321",
				"PIXSIM_URL":            "http://pixsim:8090",
				"PROVIDER_TIMEOUT":      "2s",
				"PIXSIM_WEBHOOK_SECRET": "s3cret",
				"WEBHOOK_SIGNING_KEY":   "0123456789abcdef0123456789abcdef",
				"WEBHOOK_ALLOW_PRIVATE": "true",
			},
			want: Config{
				HTTPAddr:            "127.0.0.1:9000",
				LogLevel:            slog.LevelDebug,
				ShutdownTimeout:     30 * time.Second,
				DatabaseURL:         "postgres://localhost/cbp",
				USDBRLRate:          "5.4321",
				PixSimURL:           "http://pixsim:8090",
				ProviderTimeout:     2 * time.Second,
				PixSimWebhookSecret: "s3cret",
				WebhookSigningKey:   "0123456789abcdef0123456789abcdef",
				WebhookAllowPrivate: true,
			},
		},
		{
			name: "level is case insensitive",
			vars: map[string]string{"LOG_LEVEL": "WARN"},
			want: Config{HTTPAddr: ":8080", LogLevel: slog.LevelWarn, ShutdownTimeout: 10 * time.Second, USDBRLRate: "5.0000", PixSimURL: "http://localhost:8090", ProviderTimeout: 5 * time.Second},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Load(env(tt.vars))
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("Load() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestLoadRejectsInvalid(t *testing.T) {
	tests := []struct {
		name     string
		vars     map[string]string
		wantVars []string
	}{
		{name: "address without port", vars: map[string]string{"HTTP_ADDR": "localhost"}, wantVars: []string{"HTTP_ADDR"}},
		{name: "unknown level", vars: map[string]string{"LOG_LEVEL": "loud"}, wantVars: []string{"LOG_LEVEL"}},
		{name: "unparsable timeout", vars: map[string]string{"SHUTDOWN_TIMEOUT": "ten"}, wantVars: []string{"SHUTDOWN_TIMEOUT"}},
		{name: "zero timeout", vars: map[string]string{"SHUTDOWN_TIMEOUT": "0s"}, wantVars: []string{"SHUTDOWN_TIMEOUT"}},
		{name: "negative timeout", vars: map[string]string{"SHUTDOWN_TIMEOUT": "-1s"}, wantVars: []string{"SHUTDOWN_TIMEOUT"}},
		{name: "unparsable provider timeout", vars: map[string]string{"PROVIDER_TIMEOUT": "soon"}, wantVars: []string{"PROVIDER_TIMEOUT"}},
		{name: "unparsable private webhooks", vars: map[string]string{"WEBHOOK_ALLOW_PRIVATE": "sometimes"}, wantVars: []string{"WEBHOOK_ALLOW_PRIVATE"}},
		{name: "zero provider timeout", vars: map[string]string{"PROVIDER_TIMEOUT": "0s"}, wantVars: []string{"PROVIDER_TIMEOUT"}},
		{
			name: "every error reported at once",
			vars: map[string]string{
				"HTTP_ADDR":        "localhost",
				"LOG_LEVEL":        "loud",
				"SHUTDOWN_TIMEOUT": "ten",
			},
			wantVars: []string{"HTTP_ADDR", "LOG_LEVEL", "SHUTDOWN_TIMEOUT"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(env(tt.vars))
			if err == nil {
				t.Fatal("Load() error = nil, want error")
			}
			for _, v := range tt.wantVars {
				if !strings.Contains(err.Error(), v) {
					t.Errorf("Load() error = %q, want it to name %s", err, v)
				}
			}
		})
	}
}

func TestRequireWebhookSecret(t *testing.T) {
	if err := (&Config{}).RequireWebhookSecret(); !errors.Is(err, ErrMissingWebhookSecret) {
		t.Errorf("RequireWebhookSecret() without secret = %v, want %v", err, ErrMissingWebhookSecret)
	}
	if err := (&Config{PixSimWebhookSecret: "s3cret"}).RequireWebhookSecret(); err != nil {
		t.Errorf("RequireWebhookSecret() with secret = %v", err)
	}
}

func TestRequireSigningKey(t *testing.T) {
	for key, want := range map[string]error{
		"":                                 ErrWeakSigningKey,
		"0123456789abcdef0123456789abcde":  ErrWeakSigningKey,
		"0123456789abcdef0123456789abcdef": nil,
	} {
		if err := (&Config{WebhookSigningKey: key}).RequireSigningKey(); !errors.Is(err, want) {
			t.Errorf("RequireSigningKey() with %d bytes = %v, want %v", len(key), err, want)
		}
	}
}

func TestRequireDatabase(t *testing.T) {
	if err := (&Config{}).RequireDatabase(); !errors.Is(err, ErrMissingDatabaseURL) {
		t.Errorf("RequireDatabase() without URL = %v, want %v", err, ErrMissingDatabaseURL)
	}
	if err := (&Config{DatabaseURL: "postgres://localhost/cbp"}).RequireDatabase(); err != nil {
		t.Errorf("RequireDatabase() with URL = %v, want nil", err)
	}
}
