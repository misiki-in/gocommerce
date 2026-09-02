package gocommerce

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	// pgx's database/sql driver. The engine uses database/sql with explicit
	// SQL rather than an ORM: data ownership stays visible, and no library's
	// behaviour becomes part of the architecture.
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Connection pool sizing. PostgreSQL removes the single-writer ceiling that
// an embedded database imposes, so there is no application-level write
// serialization here — concurrency control is the database's job, via row
// locks taken inside transactions.
const (
	maxOpenConns    = 25
	maxIdleConns    = 5
	connMaxLifetime = time.Hour
	connMaxIdleTime = 5 * time.Minute
	pingTimeout     = 10 * time.Second
)

// OpenDB opens and verifies a PostgreSQL connection pool.
func OpenDB(ctx context.Context, dsn string) (*sql.DB, error) {
	if dsn == "" {
		return nil, errors.New("gocommerce: empty database URL")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("gocommerce: open database: %w", err)
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxIdleConns)
	db.SetConnMaxLifetime(connMaxLifetime)
	db.SetConnMaxIdleTime(connMaxIdleTime)

	pctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("gocommerce: connect to database: %w", err)
	}
	return db, nil
}

// InTx runs fn inside a transaction, committing on success and rolling back
// on error or panic. A panic is re-raised after the rollback so a bug never
// silently leaves a transaction open.
//
// This is the only sanctioned way to write core state: a business change and
// the durable event describing it must commit together or not at all.
func InTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	return InTxOpts(ctx, db, nil, fn)
}

// InTxOpts is InTx with explicit transaction options, for the rare caller
// that needs a stricter isolation level.
func InTxOpts(ctx context.Context, db *sql.DB, opts *sql.TxOptions, fn func(*sql.Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
				err = fmt.Errorf("%w (rollback: %v)", err, rbErr)
			}
		}
	}()

	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// dbReady reports whether the database answers a trivial read. It backs the
// readiness probe, which must distinguish "the process is up" from "the
// process can serve traffic".
func dbReady(ctx context.Context, db *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var one int
	if err := db.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		return err
	}
	if one != 1 {
		return errors.New("unexpected result from readiness query")
	}
	return nil
}
