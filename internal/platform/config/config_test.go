package config

import (
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
			want: Config{HTTPAddr: ":8080", LogLevel: slog.LevelInfo, ShutdownTimeout: 10 * time.Second},
		},
		{
			name: "all set",
			vars: map[string]string{
				"HTTP_ADDR":        "127.0.0.1:9000",
				"LOG_LEVEL":        "debug",
				"SHUTDOWN_TIMEOUT": "30s",
			},
			want: Config{HTTPAddr: "127.0.0.1:9000", LogLevel: slog.LevelDebug, ShutdownTimeout: 30 * time.Second},
		},
		{
			name: "level is case insensitive",
			vars: map[string]string{"LOG_LEVEL": "WARN"},
			want: Config{HTTPAddr: ":8080", LogLevel: slog.LevelWarn, ShutdownTimeout: 10 * time.Second},
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
