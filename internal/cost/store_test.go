package cost

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestPostgresIdempotentRollups(t *testing.T) {
	dsn := os.Getenv("COST_TEST_DSN")
	if dsn == "" {
		t.Skip("set COST_TEST_DSN to an isolated PostgreSQL database")
	}
	ctx := context.Background()
	s, e := Open(ctx, dsn, true)
	if e != nil {
		t.Fatal(e)
	}
	defer s.DB.Close()
	if _, e = s.DB.ExecContext(ctx, "TRUNCATE cost_hours,cost_days,cost_exports"); e != nil {
		t.Fatal(e)
	}
	rows := []Row{{Organization: "a", Project: "p", Service: "db", Currency: "USD", Cost: 1}, {Organization: "b", Project: "secret", Cost: 99}}
	for _, h := range []time.Time{hour, hour, hour.Add(time.Hour)} {
		if e = s.SaveHour(ctx, h, rows); e != nil {
			t.Fatal(e)
		}
	}
	out, e := s.Query(ctx, "a", Day(hour), Day(hour).AddDate(0, 0, 1), "project")
	if e != nil || len(out) != 1 || out[0].Cost != 2 || out[0].SampledHours != 2 || out[0].Group != "p" {
		t.Fatalf("%+v %v", out, e)
	}
	daily, n, e := s.Daily(ctx, Day(hour))
	if e != nil || n != 2 || sum(daily) != 200 {
		t.Fatal(daily, n, e)
	}
	if e = s.MarkExported(ctx, Day(hour)); e != nil {
		t.Fatal(e)
	}
	days, e := s.PendingExports(ctx)
	if e != nil || len(days) != 0 {
		t.Fatal(days, e)
	}
}
