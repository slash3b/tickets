// Package pgtest hands every test package its own Postgres database.
//
// WHY THIS EXISTS. Each package's test helper used to connect straight to
// DATABASE_URL, apply its schema and TRUNCATE. That is correct in isolation and
// wrong the moment two packages run at once — which is the DEFAULT, because
// `go test ./...` runs packages in parallel up to GOMAXPROCS. One package's
// TRUNCATE lands in the middle of another's test:
//
//	--- FAIL: TestBuyATicketEndToEnd    order state = "unknown", want confirmed
//	--- FAIL: TestSweepExpiredIgnoresConvertingHolds    held = 0, want 2
//	--- FAIL: TestReconcilerRecoversATimedOutCharge     panic: nil pointer
//
// Every one of those passes alone and passes under `-p 1`. A suite that fails
// only under parallelism teaches people to re-run it until it goes green, which
// costs far more than the bug ever did.
//
// `-p 1` would also have fixed it, by making the whole suite serial forever. A
// database per package fixes it by removing the sharing instead.
package pgtest

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Skip is what a test sees when there is no database to talk to. Tests SKIP
// rather than fail, so `go test ./...` stays green on a machine with no Postgres
// — the tests that need one say so plainly instead of failing for a reason
// unrelated to the code.
const Skip = `DATABASE_URL not set. Start a throwaway Postgres with:

  docker run -d --name tickets-pgtest -e POSTGRES_USER=tickets \
    -e POSTGRES_PASSWORD=tickets -e POSTGRES_DB=tickets -p 55432:5432 postgres:18-alpine
  export DATABASE_URL='postgres://tickets:tickets@127.0.0.1:55432/tickets?sslmode=disable'`

// DSN returns a connection string for a database of this package's own, creating
// it on first use. name should be the package under test: "catalog", "orders".
//
// The database PERSISTS between runs. Creating it is the slow part and each
// package still truncates what it owns, so there is nothing to gain from
// dropping it and a rerun to pay for if we did.
func DSN(t *testing.T, name string) string {
	t.Helper()

	base := os.Getenv("DATABASE_URL")
	if base == "" {
		t.Skip(Skip)
	}

	dbName := "tickets_test_" + name
	target, err := withDatabase(base, dbName)
	if err != nil {
		t.Fatalf("DATABASE_URL is not a URL this can rewrite (%v).\n"+
			"It must look like postgres://user:pass@host:port/db?sslmode=disable", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := create(ctx, base, dbName); err != nil {
		t.Fatalf("create database %s: %v", dbName, err)
	}
	return target
}

// withDatabase swaps the database out of a postgres URL, leaving credentials,
// host and query parameters (sslmode above all) exactly as they were.
func withDatabase(dsn, db string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", fmt.Errorf("scheme is %q, want postgres", u.Scheme)
	}
	u.Path = "/" + db
	return u.String(), nil
}

// create makes the database if it is not already there.
//
// TWO ERRORS ARE EXPECTED HERE AND NEITHER IS A FAILURE.
//
// 42P04 duplicate_database: another package created it a moment ago, or a
// previous run did. CREATE DATABASE has no IF NOT EXISTS, so this is how you
// spell it.
//
// 55006 object_in_use: "source database template1 is being accessed by other
// users". Postgres copies a template to make a database and refuses while
// another CREATE DATABASE is reading it — which is exactly what six packages
// starting together do. It is transient by definition, so retry.
func create(ctx context.Context, adminDSN, db string) error {
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()

	var last error
	for attempt := range 10 {
		_, err := conn.Exec(ctx, `CREATE DATABASE "`+db+`"`)
		if err == nil {
			return nil
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			if pgErr.Code == "42P04" {
				return nil
			}
			if pgErr.Code == "55006" {
				last = err
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Duration(attempt+1) * 150 * time.Millisecond):
				}
				continue
			}
		}
		return err
	}
	return fmt.Errorf("template still busy after 10 attempts: %w", last)
}
