package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"
)

var ErrMissingDatabaseURL = errors.New("DATABASE_URL is required")

type Config struct {
	HTTPAddr        string
	LogLevel        slog.Level
	ShutdownTimeout time.Duration
	DatabaseURL     string
	// USDBRLRate is the fixed base rate of decision 12, as a decimal string; fx parses
	// it so that this package does not depend on a domain module.
	USDBRLRate string
	// PixSimURL is where the PIX simulator answers; every key mode uses it until a
	// real provider arrives in F9.
	PixSimURL string
	// ProviderTimeout bounds each call to a provider, below the server's write timeout
	// so that the answer can still be stored and sent.
	ProviderTimeout time.Duration
}

func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		HTTPAddr:        ":8080",
		LogLevel:        slog.LevelInfo,
		ShutdownTimeout: 10 * time.Second,
		USDBRLRate:      "5.0000",
		PixSimURL:       "http://localhost:8090",
		ProviderTimeout: 5 * time.Second,
	}
	var errs []error

	cfg.DatabaseURL = getenv("DATABASE_URL")

	if v := getenv("HTTP_ADDR"); v != "" {
		if _, _, err := net.SplitHostPort(v); err != nil {
			errs = append(errs, fmt.Errorf("HTTP_ADDR: %w", err))
		}
		cfg.HTTPAddr = v
	}

	if v := getenv("LOG_LEVEL"); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
		}
	}

	if v := getenv("SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("SHUTDOWN_TIMEOUT: %w", err))
		case d <= 0:
			errs = append(errs, fmt.Errorf("SHUTDOWN_TIMEOUT: must be positive, got %s", d))
		default:
			cfg.ShutdownTimeout = d
		}
	}

	if v := getenv("FX_USD_BRL_RATE"); v != "" {
		cfg.USDBRLRate = v
	}

	if v := getenv("PIXSIM_URL"); v != "" {
		cfg.PixSimURL = v
	}

	if v := getenv("PROVIDER_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("PROVIDER_TIMEOUT: %w", err))
		case d <= 0:
			errs = append(errs, fmt.Errorf("PROVIDER_TIMEOUT: must be positive, got %s", d))
		default:
			cfg.ProviderTimeout = d
		}
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) RequireDatabase() error {
	if c.DatabaseURL == "" {
		return ErrMissingDatabaseURL
	}
	return nil
}
