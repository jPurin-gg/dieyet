package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jPurin-gg/dieyet/internal/testdb"
)

func open(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), testdb.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func f(v float64) *float64 { return &v }

var now = time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)

func TestMigrateIsIdempotent(t *testing.T) {
	dsn := testdb.DSN(t)
	for i := 0; i < 2; i++ {
		st, err := Open(context.Background(), dsn)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		st.Close()
	}
}

func TestUsersAndSessions(t *testing.T) {
	ctx := context.Background()
	st := open(t)

	u, err := st.CreateUser(ctx, "ぷりん", "hash", now)
	if err != nil {
		t.Fatal(err)
	}
	u2, err := st.CreateUser(ctx, "ぷりん", "hash2", now)
	if err != nil {
		t.Fatal("duplicate nicknames must be allowed:", err)
	}
	creds, err := st.UsersByNickname(ctx, "ぷりん")
	if err != nil || len(creds) != 2 || creds[0].ID != u.ID || creds[1].ID != u2.ID || creds[1].Hash != "hash2" || creds[0].CreatedAt != "2026-10-20T03:00:00Z" {
		t.Fatalf("UsersByNickname = %+v, %v", creds, err)
	}

	if err := st.CreateSession(ctx, "tok", u.ID, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := st.UserBySession(ctx, "tok", now.Add(time.Minute))
	if err != nil || got.ID != u.ID || got.Nickname != "ぷりん" {
		t.Fatalf("UserBySession = %+v, %v", got, err)
	}
	if _, err := st.UserBySession(ctx, "tok", now.Add(2*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session: err = %v", err)
	}
	if _, err := st.UserBySession(ctx, "tok", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired session should have been deleted")
	}
	if _, err := st.UserBySession(ctx, "nope", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session: err = %v", err)
	}
}

func TestProductsLogsAndTotals(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	alice, _ := st.CreateUser(ctx, "alice", "h", now)
	bob, _ := st.CreateUser(ctx, "bob", "h", now)

	if _, err := st.Product(ctx, "4901777018686"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing product: err = %v", err)
	}
	p := Product{Barcode: "4901777018686", Name: "おにぎり", Unit: "1個", Source: "manual",
		Nutrients: Nutrients{Kcal: f(180), Protein: f(4.5), Fat: f(1.2), Carbs: f(38)}} // salt unknown
	if _, err := st.PutProduct(ctx, p, alice.ID, now); err != nil {
		t.Fatal(err)
	}
	got, err := st.Product(ctx, "4901777018686")
	if err != nil || got.Name != "おにぎり" || got.Kcal == nil || *got.Kcal != 180 || got.Salt != nil || got.UpdatedAt != "2026-10-20T03:00:00Z" {
		t.Fatalf("Product = %+v, %v", got, err)
	}
	// Editing a product must not touch what was already logged.
	code := "4901777018686"
	l1, err := st.AddLog(ctx, NewLog{UserID: alice.ID, Barcode: &code, Name: got.Name, Unit: got.Unit, Servings: 2,
		EatenOn: "2026-10-20", EatenAt: now, Nutrients: got.Nutrients.Scale(2)}, now)
	if err != nil {
		t.Fatal(err)
	}
	if l1.ID == 0 || l1.Kcal == nil || *l1.Kcal != 360 || l1.Salt != nil {
		t.Fatalf("scaled log = %+v", l1)
	}
	p.Kcal = f(999)
	if _, err := st.PutProduct(ctx, p, alice.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddLog(ctx, NewLog{UserID: alice.ID, Name: "学食カレー", Unit: "1食", Servings: 1, EatenOn: "2026-10-20", EatenAt: now.Add(time.Hour),
		Nutrients: Nutrients{Kcal: f(700), Salt: f(3.1)}}, now); err != nil {
		t.Fatal(err)
	}
	// Different day and different user must stay invisible.
	st.AddLog(ctx, NewLog{UserID: alice.ID, Name: "昨日", Unit: "1食", Servings: 1, EatenOn: "2026-10-19", EatenAt: now, Nutrients: Nutrients{Kcal: f(1)}}, now)
	st.AddLog(ctx, NewLog{UserID: bob.ID, Name: "bobの", Unit: "1食", Servings: 1, EatenOn: "2026-10-20", EatenAt: now, Nutrients: Nutrients{Kcal: f(1)}}, now)

	logs, err := st.LogsForDay(ctx, alice.ID, "2026-10-20")
	if err != nil || len(logs) != 2 || logs[0].ID != l1.ID || *logs[0].Kcal != 360 || logs[0].Barcode == nil || *logs[0].Barcode != code || logs[1].Barcode != nil {
		t.Fatalf("LogsForDay = %+v, %v", logs, err)
	}
	if logs[0].EatenOn != "2026-10-20" || logs[0].EatenAt != "2026-10-20T03:00:00Z" || logs[1].EatenAt != "2026-10-20T04:00:00Z" {
		t.Fatalf("dates = %s %s / %s", logs[0].EatenOn, logs[0].EatenAt, logs[1].EatenAt)
	}
	tot := Summarize(logs)
	if tot.Entries != 2 || tot.Kcal.Sum != 1060 || tot.Kcal.Unknown != 0 || tot.Salt.Sum != 3.1 || tot.Salt.Unknown != 1 || tot.Protein.Unknown != 1 || tot.Complete {
		t.Fatalf("Summarize = %+v", tot)
	}

	if err := st.DeleteLog(ctx, bob.ID, l1.ID, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other user's delete: err = %v", err)
	}
	if err := st.DeleteLog(ctx, alice.ID, l1.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteLog(ctx, alice.ID, l1.ID, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete: err = %v", err)
	}
	logs, _ = st.LogsForDay(ctx, alice.ID, "2026-10-20")
	if len(logs) != 1 || logs[0].Name != "学食カレー" {
		t.Fatalf("after delete = %+v", logs)
	}
}

// TestLeastPrivilegeRole runs the store as the production role: the workout
// app owns users, and dieyet only has the grants listed in the README.
func TestLeastPrivilegeRole(t *testing.T) {
	ctx := context.Background()
	adminDSN := testdb.DSN(t)
	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })

	u, _ := url.Parse(adminDSN)
	dbName := strings.TrimPrefix(u.Path, "/")
	raw := make([]byte, 4)
	rand.Read(raw)
	role := "dieyet_" + hex.EncodeToString(raw)
	for _, stmt := range []string{
		// what the workout app's database already has
		`CREATE TABLE users (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, nickname text NOT NULL, pass_hash text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), last_login_at timestamptz)`,
		`CREATE INDEX users_nickname ON users (nickname)`,
		// the grants from README.md, nothing more
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD 'secret'`, role),
		fmt.Sprintf(`GRANT CONNECT ON DATABASE "%s" TO %s`, dbName, role),
		fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, REFERENCES ON users TO %s`, role),
		fmt.Sprintf(`GRANT USAGE, SELECT ON SEQUENCE users_id_seq TO %s`, role),
		fmt.Sprintf(`CREATE SCHEMA food AUTHORIZATION %s`, role),
	} {
		if _, err := admin.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		admin.ExecContext(ctx, fmt.Sprintf(`DROP OWNED BY %s`, role))
		admin.ExecContext(ctx, fmt.Sprintf(`DROP ROLE %s`, role))
	})

	u.User = url.UserPassword(role, "secret")
	st, err := Open(ctx, u.String())
	if err != nil {
		t.Fatalf("open as %s: %v", role, err)
	}
	t.Cleanup(func() { st.Close() })

	alice, err := st.CreateUser(ctx, "alice", "h", now)
	if err != nil {
		t.Fatal("create user:", err)
	}
	if err := st.CreateSession(ctx, "tok", alice.ID, now, now.Add(time.Hour)); err != nil {
		t.Fatal("create session:", err)
	}
	if _, err := st.UserBySession(ctx, "tok", now); err != nil {
		t.Fatal("user by session:", err)
	}
	if _, err := st.PutProduct(ctx, Product{Barcode: "4901777018686", Name: "x", Unit: "1個", Source: "manual"}, alice.ID, now); err != nil {
		t.Fatal("put product:", err)
	}
	l, err := st.AddLog(ctx, NewLog{UserID: alice.ID, Name: "x", Unit: "1食", Servings: 1, EatenOn: "2026-10-20", EatenAt: now}, now)
	if err != nil {
		t.Fatal("add log:", err)
	}
	if err := st.DeleteLog(ctx, alice.ID, l.ID, now); err != nil {
		t.Fatal("delete log:", err)
	}
	// Pruning deletes from users, which this role deliberately cannot do.
	if _, err := st.PruneIdleUsers(ctx, now); err == nil {
		t.Fatal("prune should be denied for the least-privilege role")
	}
	// The workout app can read what was logged.
	var n int
	if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM food.logs l JOIN users u ON u.id = l.user_id WHERE u.nickname = 'alice'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("workout-side read: n=%d err=%v", n, err)
	}
	// A restart (schema and migrations already present) must also work.
	st2, err := Open(ctx, u.String())
	if err != nil {
		t.Fatalf("reopen as %s: %v", role, err)
	}
	st2.Close()
}

func TestSummarizeEmpty(t *testing.T) {
	tot := Summarize(nil)
	if tot.Entries != 0 || !tot.Complete || tot.Kcal.Sum != 0 {
		t.Fatalf("Summarize(nil) = %+v", tot)
	}
}

func TestPruneIdleUsers(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	old := now.Add(-40 * 24 * time.Hour)
	idle, _ := st.CreateUser(ctx, "idle", "h", old)
	active, _ := st.CreateUser(ctx, "active", "h", old)
	st.CreateUser(ctx, "fresh", "h", now)
	st.AddLog(ctx, NewLog{UserID: active.ID, Name: "x", Unit: "1食", Servings: 1, EatenOn: "2026-10-01", EatenAt: old}, old)
	st.CreateSession(ctx, "idle-session", idle.ID, now, now.Add(time.Hour))

	n, err := st.PruneIdleUsers(ctx, now.Add(-30*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	for _, c := range []struct {
		name string
		want int
	}{{"idle", 0}, {"active", 1}, {"fresh", 1}} {
		creds, _ := st.UsersByNickname(ctx, c.name)
		if len(creds) != c.want {
			t.Fatalf("%s: %d users, want %d", c.name, len(creds), c.want)
		}
	}
	if _, err := st.UserBySession(ctx, "idle-session", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("session of pruned user should be gone (ON DELETE CASCADE)")
	}
}
