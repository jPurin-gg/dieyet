// Package testdb hands each test a fresh PostgreSQL database.
package testdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// DSN creates an empty database on the server named by DIEYET_TEST_DATABASE_URL
// and returns a connection string for it. The database is dropped when the
// test ends. Tests are skipped when the variable is not set.
func DSN(t *testing.T) string {
	t.Helper()
	admin := os.Getenv("DIEYET_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("DIEYET_TEST_DATABASE_URL not set (see README: テスト)")
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("DIEYET_TEST_DATABASE_URL: %v", err)
	}
	db, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	raw := make([]byte, 6)
	rand.Read(raw)
	name := "t_" + hex.EncodeToString(raw)
	if _, err := db.ExecContext(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		db.Close()
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(ctx, `DROP DATABASE "`+name+`" WITH (FORCE)`); err != nil {
			t.Logf("drop test database %s: %v", name, err)
		}
		db.Close()
	})
	u.Path = "/" + name
	return u.String()
}
