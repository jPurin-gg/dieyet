// Package store is the PostgreSQL persistence layer: schema migrations for the
// food schema and the queries the HTTP layer needs. Nutrient values are
// *float64 everywhere so that "unknown" (NULL) is never confused with 0.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"sort"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// ErrNotFound is returned when a row does not exist or is not visible to the caller.
var ErrNotFound = errors.New("not found")

// ErrConnect wraps failures to reach the database, as opposed to migration
// errors, so a caller can retry only the former while PostgreSQL boots.
var ErrConnect = errors.New("connect to postgres")

type Store struct {
	db *sql.DB
}

// Open connects to PostgreSQL (dsn is a postgres:// URL) and applies this
// app's migrations. The users table is shared with the workout app and is
// only created when it does not exist yet.
func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(time.Hour)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("%w: %v", ErrConnect, err)
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// usersDDL is the shape of the users table both apps agreed on. In production
// the workout app owns the table; this is only run on a database without one
// (tests, running dieyet on its own).
const usersDDL = `
CREATE TABLE users (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  nickname      text NOT NULL,
  pass_hash     text NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  last_login_at timestamptz
);
CREATE INDEX users_nickname ON users (nickname)`

func migrate(ctx context.Context, db *sql.DB) error {
	// Existence checks instead of IF NOT EXISTS: PostgreSQL checks the CREATE
	// privilege before the existence of the object, and the dieyet role has no
	// CREATE on the database or on the public schema in production.
	var schemaExists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'food')`).Scan(&schemaExists); err != nil {
		return err
	}
	if !schemaExists {
		if _, err := db.ExecContext(ctx, `CREATE SCHEMA food`); err != nil {
			return fmt.Errorf("create schema food (or create it beforehand with AUTHORIZATION dieyet): %w", err)
		}
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS food.schema_migrations (version text PRIMARY KEY, applied_at timestamptz NOT NULL)`); err != nil {
		return fmt.Errorf("migrations table: %w", err)
	}
	var usersExists bool
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('public.users') IS NOT NULL`).Scan(&usersExists); err != nil {
		return err
	}
	if !usersExists {
		for _, stmt := range splitStatements(usersDDL) {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("create users table: %w", err)
			}
		}
	}
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Two instances starting at once must not both apply the same migration.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(2026_1020)`); err != nil {
		return err
	}
	for _, name := range names {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM food.schema_migrations WHERE version = $1`, name).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		script, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		for _, stmt := range splitStatements(string(script)) {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("migration %s: %w\n%s", name, err, stmt)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO food.schema_migrations (version, applied_at) VALUES ($1, $2)`, name, time.Now().UTC()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// splitStatements splits a migration file on ";" after dropping comment lines.
// Good enough for the DDL here (no functions, no ";" inside strings).
func splitStatements(script string) []string {
	var lines []string
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		lines = append(lines, line)
	}
	var out []string
	for _, stmt := range strings.Split(strings.Join(lines, "\n"), ";") {
		if stmt = strings.TrimSpace(stmt); stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// ---------------------------------------------------------------- users

type User struct {
	ID        int64  `json:"id"`
	Nickname  string `json:"nickname"`
	CreatedAt string `json:"created_at"`
}

// Credential is a user together with the passphrase hash needed to verify a login.
type Credential struct {
	User
	Hash string // PHC string
}

// UsersByNickname returns every account with this nickname; the caller picks
// the one whose passphrase matches.
func (s *Store) UsersByNickname(ctx context.Context, nickname string) ([]Credential, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, nickname, created_at, pass_hash FROM users WHERE nickname = $1 ORDER BY id`, nickname)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Credential
	for rows.Next() {
		var c Credential
		var created time.Time
		if err := rows.Scan(&c.ID, &c.Nickname, &created, &c.Hash); err != nil {
			return nil, err
		}
		c.CreatedAt = fmtTime(created)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) CreateUser(ctx context.Context, nickname, hash string, now time.Time) (User, error) {
	u := User{Nickname: nickname, CreatedAt: fmtTime(now)}
	err := s.db.QueryRowContext(ctx, `INSERT INTO users (nickname, pass_hash, created_at, last_login_at) VALUES ($1, $2, $3, $3) RETURNING id`,
		nickname, hash, now.UTC()).Scan(&u.ID)
	if err != nil {
		return User{}, err
	}
	return u, nil
}

func (s *Store) TouchLogin(ctx context.Context, userID int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET last_login_at = $1 WHERE id = $2`, now.UTC(), userID)
	return err
}

// PruneIdleUsers deletes accounts created before cutoff that never logged any
// food (typos on the login screen create such accounts by design). It only
// looks at this app's tables, so leave it disabled when users is shared with
// another app that may hold data for those accounts.
func (s *Store) PruneIdleUsers(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE created_at < $1 AND NOT EXISTS (SELECT 1 FROM food.logs WHERE food.logs.user_id = users.id)`, cutoff.UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------- sessions

func (s *Store) CreateSession(ctx context.Context, id string, userID int64, now, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO food.sessions (id, user_id, created_at, expires_at, last_seen_at) VALUES ($1, $2, $3, $4, $3)`, id, userID, now.UTC(), expires.UTC())
	return err
}

// UserBySession resolves a session id to its user, or ErrNotFound when the
// session is missing or expired.
func (s *Store) UserBySession(ctx context.Context, id string, now time.Time) (User, error) {
	var u User
	var created, expires time.Time
	err := s.db.QueryRowContext(ctx, `SELECT u.id, u.nickname, u.created_at, s.expires_at FROM food.sessions s JOIN users u ON u.id = s.user_id WHERE s.id = $1`, id).
		Scan(&u.ID, &u.Nickname, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	if !now.Before(expires) {
		_ = s.DeleteSession(ctx, id)
		return User{}, ErrNotFound
	}
	u.CreatedAt = fmtTime(created)
	// Record activity at most every 10 minutes to avoid a write per request.
	_, _ = s.db.ExecContext(ctx, `UPDATE food.sessions SET last_seen_at = $1 WHERE id = $2 AND last_seen_at < $3`, now.UTC(), id, now.Add(-10*time.Minute).UTC())
	return u, nil
}

func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM food.sessions WHERE id = $1`, id)
	return err
}

func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM food.sessions WHERE expires_at <= $1`, now.UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------- products

// Nutrients holds the five values Japanese labels must show. nil = unknown.
type Nutrients struct {
	Kcal    *float64 `json:"kcal"`
	Protein *float64 `json:"protein_g"`
	Fat     *float64 `json:"fat_g"`
	Carbs   *float64 `json:"carbs_g"`
	Salt    *float64 `json:"salt_g"`
}

// Scale returns the nutrients multiplied by servings (unknown stays unknown).
func (n Nutrients) Scale(servings float64) Nutrients {
	mul := func(v *float64) *float64 {
		if v == nil {
			return nil
		}
		x := Round2(*v * servings)
		return &x
	}
	return Nutrients{Kcal: mul(n.Kcal), Protein: mul(n.Protein), Fat: mul(n.Fat), Carbs: mul(n.Carbs), Salt: mul(n.Salt)}
}

type Product struct {
	Barcode string `json:"barcode"`
	Name    string `json:"name"`
	Brand   string `json:"brand"`
	Unit    string `json:"unit"` // what the nutrients refer to: "1個", "1袋(60g)", "100g"
	Nutrients
	Source    string `json:"source"` // "manual" | "openfoodfacts"
	UpdatedAt string `json:"updated_at"`
}

func (s *Store) Product(ctx context.Context, barcode string) (Product, error) {
	var p Product
	var kcal, pro, fat, carb, salt sql.NullFloat64
	var updated time.Time
	err := s.db.QueryRowContext(ctx, `SELECT barcode, name, brand, unit, kcal, protein_g, fat_g, carbs_g, salt_g, source, updated_at FROM food.products WHERE barcode = $1`, barcode).
		Scan(&p.Barcode, &p.Name, &p.Brand, &p.Unit, &kcal, &pro, &fat, &carb, &salt, &p.Source, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Product{}, ErrNotFound
	}
	if err != nil {
		return Product{}, err
	}
	p.Nutrients = Nutrients{Kcal: ptr(kcal), Protein: ptr(pro), Fat: ptr(fat), Carbs: ptr(carb), Salt: ptr(salt)}
	p.UpdatedAt = fmtTime(updated)
	return p, nil
}

// PutProduct inserts or replaces the product for p.Barcode.
func (s *Store) PutProduct(ctx context.Context, p Product, userID int64, now time.Time) (Product, error) {
	p.UpdatedAt = fmtTime(now)
	_, err := s.db.ExecContext(ctx, `INSERT INTO food.products (barcode, name, brand, unit, kcal, protein_g, fat_g, carbs_g, salt_g, source, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (barcode) DO UPDATE SET
			name = excluded.name, brand = excluded.brand, unit = excluded.unit,
			kcal = excluded.kcal, protein_g = excluded.protein_g, fat_g = excluded.fat_g,
			carbs_g = excluded.carbs_g, salt_g = excluded.salt_g, source = excluded.source,
			updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
		p.Barcode, p.Name, p.Brand, p.Unit, p.Kcal, p.Protein, p.Fat, p.Carbs, p.Salt, p.Source, userID, now.UTC())
	if err != nil {
		return Product{}, err
	}
	return p, nil
}

// ---------------------------------------------------------------- logs

type Log struct {
	ID       int64   `json:"id"`
	Barcode  *string `json:"barcode"`
	Name     string  `json:"name"`
	Unit     string  `json:"unit"`
	Servings float64 `json:"servings"`
	EatenOn  string  `json:"eaten_on"` // YYYY-MM-DD, the phone's local date
	EatenAt  string  `json:"eaten_at"` // RFC 3339 UTC
	Nutrients
}

type NewLog struct {
	UserID   int64
	Barcode  *string
	Name     string
	Unit     string
	Servings float64
	EatenOn  string // YYYY-MM-DD
	EatenAt  time.Time
	Nutrients
}

func (s *Store) AddLog(ctx context.Context, n NewLog, now time.Time) (Log, error) {
	day, err := time.Parse("2006-01-02", n.EatenOn)
	if err != nil {
		return Log{}, fmt.Errorf("eaten_on: %w", err)
	}
	l := Log{Barcode: n.Barcode, Name: n.Name, Unit: n.Unit, Servings: n.Servings, EatenOn: n.EatenOn, EatenAt: fmtTime(n.EatenAt), Nutrients: n.Nutrients}
	err = s.db.QueryRowContext(ctx, `INSERT INTO food.logs (user_id, barcode, name, unit, servings, eaten_on, eaten_at, kcal, protein_g, fat_g, carbs_g, salt_g, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING id`,
		n.UserID, n.Barcode, n.Name, n.Unit, n.Servings, day, n.EatenAt.UTC(), n.Kcal, n.Protein, n.Fat, n.Carbs, n.Salt, now.UTC()).Scan(&l.ID)
	if err != nil {
		return Log{}, err
	}
	return l, nil
}

// LogsForDay lists the user's undeleted entries for a local date, oldest first.
func (s *Store) LogsForDay(ctx context.Context, userID int64, day string) ([]Log, error) {
	d, err := time.Parse("2006-01-02", day)
	if err != nil {
		return nil, fmt.Errorf("day: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, barcode, name, unit, servings, eaten_on, eaten_at, kcal, protein_g, fat_g, carbs_g, salt_g
		FROM food.logs WHERE user_id = $1 AND eaten_on = $2 AND deleted_at IS NULL ORDER BY eaten_at, id`, userID, d)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Log{}
	for rows.Next() {
		var l Log
		var barcode sql.NullString
		var eatenOn, eatenAt time.Time
		var kcal, pro, fat, carb, salt sql.NullFloat64
		if err := rows.Scan(&l.ID, &barcode, &l.Name, &l.Unit, &l.Servings, &eatenOn, &eatenAt, &kcal, &pro, &fat, &carb, &salt); err != nil {
			return nil, err
		}
		if barcode.Valid {
			l.Barcode = &barcode.String
		}
		l.EatenOn = eatenOn.Format("2006-01-02")
		l.EatenAt = fmtTime(eatenAt)
		l.Nutrients = Nutrients{Kcal: ptr(kcal), Protein: ptr(pro), Fat: ptr(fat), Carbs: ptr(carb), Salt: ptr(salt)}
		out = append(out, l)
	}
	return out, rows.Err()
}

// DeleteLog soft-deletes one of the user's own entries.
func (s *Store) DeleteLog(ctx context.Context, userID, id int64, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE food.logs SET deleted_at = $1 WHERE id = $2 AND user_id = $3 AND deleted_at IS NULL`, now.UTC(), id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------- totals

type Total struct {
	Sum     float64 `json:"sum"`
	Unknown int     `json:"unknown"` // entries whose value for this nutrient is unknown
}

type Totals struct {
	Kcal     Total `json:"kcal"`
	Protein  Total `json:"protein_g"`
	Fat      Total `json:"fat_g"`
	Carbs    Total `json:"carbs_g"`
	Salt     Total `json:"salt_g"`
	Entries  int   `json:"entries"`
	Complete bool  `json:"complete"` // false when any entry has any unknown value
}

// Summarize adds up a day's entries. Unknown values are counted, never treated as 0.
func Summarize(logs []Log) Totals {
	t := Totals{Entries: len(logs), Complete: true}
	add := func(tot *Total, v *float64) {
		if v == nil {
			tot.Unknown++
			t.Complete = false
			return
		}
		tot.Sum += *v
	}
	for _, l := range logs {
		add(&t.Kcal, l.Kcal)
		add(&t.Protein, l.Protein)
		add(&t.Fat, l.Fat)
		add(&t.Carbs, l.Carbs)
		add(&t.Salt, l.Salt)
	}
	for _, tot := range []*Total{&t.Kcal, &t.Protein, &t.Fat, &t.Carbs, &t.Salt} {
		tot.Sum = Round2(tot.Sum)
	}
	return t
}

func Round2(v float64) float64 { return math.Round(v*100) / 100 }

func ptr(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	v := n.Float64
	return &v
}
