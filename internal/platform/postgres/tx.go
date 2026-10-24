package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const maxTxAttempts = 3

type TxFunc func(ctx context.Context, tx pgx.Tx) error

type txBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

func WithTx(ctx context.Context, db txBeginner, iso pgx.TxIsoLevel, fn TxFunc) error {
	var err error
	for range maxTxAttempts {
		err = attempt(ctx, db, iso, fn)
		if !retryable(err) {
			return err
		}
	}
	return fmt.Errorf("postgres: %d attempts: %w", maxTxAttempts, err)
}

const (
	serializationFailure = "40001"
	deadlockDetected     = "40P01"
)

func retryable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == serializationFailure || pgErr.Code == deadlockDetected
}

func attempt(ctx context.Context, db txBeginner, iso pgx.TxIsoLevel, fn TxFunc) error {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{IsoLevel: iso})
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}

	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if err := fn(ctx, tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit: %w", err)
	}

	return nil
}
