package cost

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	_ "github.com/jackc/pgx/v5/stdlib"
	"sort"
	"time"
)

//go:embed migrations/001_ledger.sql
var migration string

type Store struct{ DB *sql.DB }

func Open(ctx context.Context, dsn string, migrate bool) (*Store, error) {
	db, e := sql.Open("pgx", dsn)
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	s := &Store{db}
	if e = db.PingContext(ctx); e == nil && migrate {
		_, e = db.ExecContext(ctx, migration)
	}
	if e != nil {
		db.Close()
		return nil, e
	}
	return s, nil
}
func Day(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
func Rollup(rows []Row) []Row {
	sums := map[string]Row{}
	for _, r := range rows {
		v := r
		v.Cost = 0
		k, _ := json.Marshal(v)
		old, ok := sums[string(k)]
		if !ok {
			old = v
		}
		old.Cost += r.Cost
		sums[string(k)] = old
	}
	keys := make([]string, 0, len(sums))
	for k := range sums {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []Row{}
	for _, k := range keys {
		out = append(out, sums[k])
	}
	return out
}

// SaveHour commits an installation snapshot once. Retries and concurrent replicas
// cannot double charge or rewrite an already observed hour with later inventory.
func (s *Store) SaveHour(ctx context.Context, hour time.Time, rows []Row) error {
	if !hour.Equal(hour.Truncate(time.Hour)) {
		return fmt.Errorf("hour must be aligned")
	}
	b, e := json.Marshal(rows)
	if e != nil {
		return e
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	// Serialize daily rollups, including concurrent hourly writers.
	if _, e = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", Day(hour).Unix()); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO cost_hours(hour,rows) VALUES($1,$2) ON CONFLICT DO NOTHING", hour, b); e != nil {
		return e
	}
	rr, e := tx.QueryContext(ctx, "SELECT rows FROM cost_hours WHERE hour >= $1 AND hour < $2 ORDER BY hour", Day(hour), Day(hour).AddDate(0, 0, 1))
	if e != nil {
		return e
	}
	all := []Row{}
	count := 0
	for rr.Next() {
		var data []byte
		if e = rr.Scan(&data); e != nil {
			rr.Close()
			return e
		}
		var r []Row
		if e = json.Unmarshal(data, &r); e != nil {
			rr.Close()
			return e
		}
		all = append(all, r...)
		count++
	}
	e = rr.Err()
	rr.Close()
	if e != nil {
		return e
	}
	b, e = json.Marshal(Rollup(all))
	if e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO cost_days(day,rows,sampled_hours) VALUES($1,$2,$3) ON CONFLICT(day) DO UPDATE SET rows=excluded.rows,sampled_hours=excluded.sampled_hours,updated_at=now()", Day(hour), b, count); e != nil {
		return e
	}
	return tx.Commit()
}

type Summary struct {
	Day             string  `json:"day"`
	Group           string  `json:"group"`
	Project         string  `json:"project"`
	Currency        string  `json:"currency"`
	Cost            float64 `json:"cost"`
	UnallocatedCost float64 `json:"unallocatedCost"`
	SampledHours    int     `json:"sampledHours"`
}
type Reader interface {
	Query(context.Context, string, time.Time, time.Time, string) ([]Summary, error)
}

func (s *Store) Query(ctx context.Context, org string, from, to time.Time, group string) ([]Summary, error) {
	if group != "project" && group != "service" && group != "tier" {
		return nil, fmt.Errorf("invalid groupBy")
	}
	// Organization filtering happens in SQL before returning data to the handler.
	q := `SELECT day, COALESCE(r->>$4,''), CASE WHEN $4='service' THEN COALESCE(r->>'project','') ELSE '' END, r->>'currency', SUM((r->>'cost')::numeric), SUM(CASE WHEN (r->>'unallocated')::boolean THEN (r->>'cost')::numeric ELSE 0 END), sampled_hours FROM cost_days CROSS JOIN LATERAL jsonb_array_elements(rows) r WHERE day >= $1 AND day < $2 AND r->>'organization'=$3 GROUP BY 1,2,3,4,7 ORDER BY 1,2,3,4`
	rr, e := s.DB.QueryContext(ctx, q, from, to, org, group)
	if e != nil {
		return nil, e
	}
	defer rr.Close()
	out := []Summary{}
	for rr.Next() {
		var r Summary
		var day time.Time
		if e = rr.Scan(&day, &r.Group, &r.Project, &r.Currency, &r.Cost, &r.UnallocatedCost, &r.SampledHours); e != nil {
			return nil, e
		}
		r.Day = day.Format(time.DateOnly)
		out = append(out, r)
	}
	return out, rr.Err()
}
func (s *Store) PendingExports(ctx context.Context) ([]time.Time, error) {
	r, e := s.DB.QueryContext(ctx, "SELECT day FROM cost_days WHERE day < (now() AT TIME ZONE 'UTC')::date AND day NOT IN (SELECT day FROM cost_exports) ORDER BY day")
	if e != nil {
		return nil, e
	}
	defer r.Close()
	out := []time.Time{}
	for r.Next() {
		var t time.Time
		if e = r.Scan(&t); e != nil {
			return nil, e
		}
		out = append(out, t)
	}
	return out, r.Err()
}
func (s *Store) Daily(ctx context.Context, day time.Time) ([]Row, int, error) {
	var b []byte
	var count int
	e := s.DB.QueryRowContext(ctx, "SELECT rows,sampled_hours FROM cost_days WHERE day=$1", day).Scan(&b, &count)
	if e != nil {
		return nil, 0, e
	}
	var rows []Row
	e = json.Unmarshal(b, &rows)
	return rows, count, e
}
func (s *Store) MarkExported(ctx context.Context, day time.Time) error {
	_, e := s.DB.ExecContext(ctx, "INSERT INTO cost_exports(day) VALUES($1) ON CONFLICT DO NOTHING", day)
	return e
}
