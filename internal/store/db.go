package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/Cadoranee/Doverka/migrations"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type DB struct {
	pool *pgxpool.Pool
}

func Open(ctx context.Context, databaseURL string) (*DB, error) {
	databaseURL = strings.TrimSpace(databaseURL)
	if databaseURL == "" {
		return nil, errors.New("database URL is empty")
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	return &DB{pool: pool}, nil
}

func (db *DB) Close() {
	if db == nil || db.pool == nil {
		return
	}
	db.pool.Close()
}

func (db *DB) Pool() *pgxpool.Pool {
	if db == nil {
		return nil
	}
	return db.pool
}

func (db *DB) Ping(ctx context.Context) error {
	if db == nil || db.pool == nil {
		return errors.New("database pool is nil")
	}
	return db.pool.Ping(ctx)
}

func (db *DB) Migrate(ctx context.Context) error {
	if db == nil || db.pool == nil {
		return errors.New("database pool is nil")
	}
	_, err := db.pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
	version text PRIMARY KEY,
	applied_at timestamptz NOT NULL DEFAULT now()
)`)
	if err != nil {
		return fmt.Errorf("schema_migrations init: %w", err)
	}

	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("read migration dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		var applied bool
		err := db.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`, name).Scan(&applied)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if applied {
			continue
		}

		sqlBytes, err := migrations.FS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		sqlText := strings.TrimSpace(string(sqlBytes))

		tx, err := db.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}

		if sqlText != "" {
			if err := execSQLSimpleProtocol(ctx, tx.Conn().PgConn(), sqlText); err != nil {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("%s: %w", name, err)
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("%s: %w", name, err)
		}
	}

	return nil
}

func execSQLSimpleProtocol(ctx context.Context, pgConn *pgconn.PgConn, sql string) error {
	mrr := pgConn.Exec(ctx, sql)
	for mrr.NextResult() {
		rr := mrr.ResultReader()
		if _, err := rr.Close(); err != nil {
			_ = mrr.Close()
			return err
		}
	}
	return mrr.Close()
}
