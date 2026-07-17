// Package storage embeds the canonical SQL migrations and queries for the durable
// execution spine and opens tenant-scoped connection pools against them. The SQL
// files are the single source of truth; Go code loads statements by name rather
// than re-declaring them (spec §24.3 system of record).
package storage

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/000001_core.up.sql
var migrationUp string

//go:embed migrations/000001_core.down.sql
var migrationDown string

//go:embed migrations/000002_retention.up.sql
var migrationUp2 string

//go:embed migrations/000002_retention.down.sql
var migrationDown2 string

//go:embed queries/jobs.sql
var jobsSQL string

//go:embed queries/events.sql
var eventsSQL string

//go:embed queries/responses.sql
var responsesSQL string

//go:embed queries/identity.sql
var identitySQL string

// MigrationUp is the forward migration chain, applied in version order (000001 then
// 000002). Each file is individually idempotent, so the whole chain is safe to re-run.
func MigrationUp() string { return migrationUp + "\n" + migrationUp2 }

// MigrationDown reverses MigrationUp in the opposite order: 000002 drops its added
// columns before 000001 drops the tables that carried them.
func MigrationDown() string { return migrationDown2 + "\n" + migrationDown }

var namedQueries = parseNamedQueries(jobsSQL, eventsSQL, responsesSQL, identitySQL)

// Query returns the SQL statement labelled "-- name: <name>" in storage/queries.
// It panics on an unknown name because query names are compile-time constants.
func Query(name string) string {
	statement, ok := namedQueries[name]
	if !ok {
		panic(fmt.Sprintf("storage: unknown query %q", name))
	}
	return statement
}

// parseNamedQueries splits yesql-style "-- name: X" blocks into a name->SQL map.
func parseNamedQueries(files ...string) map[string]string {
	out := map[string]string{}
	for _, file := range files {
		name := ""
		var body strings.Builder
		flush := func() {
			if name != "" {
				out[name] = strings.TrimSpace(body.String())
			}
			body.Reset()
		}
		for _, line := range strings.Split(file, "\n") {
			if marker, ok := strings.CutPrefix(line, "-- name:"); ok {
				flush()
				name = strings.TrimSpace(marker)
				continue
			}
			if name != "" {
				body.WriteString(line)
				body.WriteByte('\n')
			}
		}
		flush()
	}
	return out
}

// OpenPool opens a verified connection pool. The URL carries a local throwaway
// credential supplied by the caller; it is never embedded here.
func OpenPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return nil, errors.New("storage: database URL is required")
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	config.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open database pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}
