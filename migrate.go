package gocommerce

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"regexp"
	"time"
)

// coreMigrationOwner is the owner name recorded for the engine's own
// migrations and used as the owner label for core-mounted routes and hooks.
const coreMigrationOwner = "core"

// migrationsTable is deliberately not the conventional "schema_migrations".
// The engine is embedded in someone else's application and may share a
// database with that application's own migration tooling; a generic name
// would eventually collide with it.
const migrationsTable = "gocommerce_migrations"

// migrationLockKey is a PostgreSQL advisory lock key ("gocommer" in ASCII).
// It serializes migration runs across application instances, so rolling out
// several nodes at once cannot apply the same migration twice.
const migrationLockKey int64 = 0x676F636F6D6D6572

// Migration is one forward schema change. Exactly one of SQL or Run must be
// set. There are no down migrations: a mistake is corrected by a new forward
// migration, which is the only thing that works under pressure anyway.
type Migration struct {
	// ID is unique within its owner and orders the migrations lexically by
	// convention, e.g. "0001_init". It is recorded as "<owner>/<id>".
	ID string
	// SQL is applied as a single statement batch inside one transaction.
	SQL string
	// Run performs a Go-level data fix inside the same transaction, for
	// changes SQL alone cannot express.
	Run func(ctx context.Context, tx *sql.Tx) error
}

type migrationSet struct {
	Owner      string
	Migrations []Migration
}

var migrationIDRE = regexp.MustCompile(`^[a-z0-9_]+$`)

func validateMigrations(sets []migrationSet) error {
	for _, set := range sets {
		seen := make(map[string]bool, len(set.Migrations))
		for i, m := range set.Migrations {
			switch {
			case m.ID == "":
				return fmt.Errorf("migration %s[%d]: empty ID", set.Owner, i)
			case !migrationIDRE.MatchString(m.ID):
				return fmt.Errorf("migration %s/%s: ID must match [a-z0-9_]+", set.Owner, m.ID)
			case seen[m.ID]:
				return fmt.Errorf("migration %s/%s: duplicate ID", set.Owner, m.ID)
			case m.SQL == "" && m.Run == nil:
				return fmt.Errorf("migration %s/%s: neither SQL nor Run is set", set.Owner, m.ID)
			case m.SQL != "" && m.Run != nil:
				return fmt.Errorf("migration %s/%s: set exactly one of SQL or Run", set.Owner, m.ID)
			}
			seen[m.ID] = true
		}
	}
	return nil
}

// runMigrations applies every pending migration in order: core's first, then
// each module's in the order the modules were passed to New. Each migration
// runs in its own transaction — PostgreSQL DDL is transactional, so a failing
// migration leaves no partial schema behind.
func runMigrations(ctx context.Context, db *sql.DB, log *slog.Logger, sets []migrationSet) error {
	if err := validateMigrations(sets); err != nil {
		return fmt.Errorf("gocommerce: %w", err)
	}

	// Hold the advisory lock on one dedicated connection for the whole run.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("gocommerce: acquire migration connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("gocommerce: acquire migration lock: %w", err)
	}
	defer func() {
		if _, err := conn.ExecContext(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock($1)`, migrationLockKey); err != nil {
			log.Error("release migration lock", "error", err)
		}
	}()

	if _, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+migrationsTable+` (
			owner      text        NOT NULL,
			id         text        NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now(),
			PRIMARY KEY (owner, id)
		)`); err != nil {
		return fmt.Errorf("gocommerce: create %s: %w", migrationsTable, err)
	}

	applied, err := appliedMigrations(ctx, conn)
	if err != nil {
		return err
	}

	var count int
	for _, set := range sets {
		for _, m := range set.Migrations {
			key := set.Owner + "/" + m.ID
			if applied[key] {
				continue
			}
			start := time.Now()
			if err := applyMigration(ctx, db, set.Owner, m); err != nil {
				return fmt.Errorf("gocommerce: migration %s: %w", key, err)
			}
			count++
			log.Info("migration applied", "migration", key, "duration", time.Since(start))
		}
	}
	if count == 0 {
		log.Debug("schema up to date")
	}
	return nil
}

func appliedMigrations(ctx context.Context, conn *sql.Conn) (map[string]bool, error) {
	rows, err := conn.QueryContext(ctx, `SELECT owner, id FROM `+migrationsTable)
	if err != nil {
		return nil, fmt.Errorf("gocommerce: read applied migrations: %w", err)
	}
	defer rows.Close()

	applied := map[string]bool{}
	for rows.Next() {
		var owner, id string
		if err := rows.Scan(&owner, &id); err != nil {
			return nil, fmt.Errorf("gocommerce: scan applied migration: %w", err)
		}
		applied[owner+"/"+id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gocommerce: read applied migrations: %w", err)
	}
	return applied, nil
}

func applyMigration(ctx context.Context, db *sql.DB, owner string, m Migration) error {
	return InTx(ctx, db, func(tx *sql.Tx) error {
		if m.SQL != "" {
			if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
				return err
			}
		} else if err := m.Run(ctx, tx); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO `+migrationsTable+` (owner, id) VALUES ($1, $2)`, owner, m.ID)
		return err
	})
}

// Migrate applies any pending migrations without starting the HTTP server.
// It backs the "gocommerce migrate" command, so a deployment can migrate as a
// separate step from serving.
func (a *App) Migrate(ctx context.Context) error { return a.migrate(ctx) }
