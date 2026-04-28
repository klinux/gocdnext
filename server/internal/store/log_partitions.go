package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// LogPartition describes one monthly child of the log_lines parent.
// Start is inclusive, End exclusive — matches FOR VALUES FROM (...)
// TO (...) on the partition.
type LogPartition struct {
	Name  string
	Start time.Time
	End   time.Time
}

// monthlyPartitionName derives the deterministic child-table name
// for a month. Lives in one place so both the migration and the
// lifecycle helpers agree on the format — `log_lines_y2026m04` for
// April 2026.
func monthlyPartitionName(monthStart time.Time) string {
	return fmt.Sprintf("log_lines_y%sm%s",
		monthStart.UTC().Format("2006"),
		monthStart.UTC().Format("01"),
	)
}

// EnsureLogPartition creates the monthly child for the given month
// if it isn't there already. Idempotent — racing ticks just lose
// the CREATE; pgerrcode 42P07 (duplicate_table) maps to "another
// caller won, post-condition met".
//
// The argument is normalised to the first day of its month UTC, so
// callers can pass mid-month times without thinking about it.
func (s *Store) EnsureLogPartition(ctx context.Context, monthStart time.Time) error {
	start := firstOfMonthUTC(monthStart)
	end := start.AddDate(0, 1, 0)
	name := monthlyPartitionName(start)

	// `IF NOT EXISTS` isn't legal syntax for CREATE TABLE … PARTITION
	// OF, so probe pg_class first and skip when present. The probe
	// is racy with another concurrent ensure call, so the CREATE is
	// also error-tolerant against duplicate_table.
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1)`,
		name,
	).Scan(&exists)
	if err != nil {
		return fmt.Errorf("log partition probe: %w", err)
	}
	if exists {
		return nil
	}

	stmt := fmt.Sprintf(
		`CREATE TABLE %s PARTITION OF log_lines FOR VALUES FROM ('%s') TO ('%s')`,
		name, start.Format(time.RFC3339), end.Format(time.RFC3339),
	)
	if _, err := s.pool.Exec(ctx, stmt); err != nil {
		var pgErr *pgconn.PgError
		// 42P07 = duplicate_table. Another sweeper raced us; post-
		// condition (partition exists) holds, so swallow.
		if errors.As(err, &pgErr) && pgErr.Code == "42P07" {
			return nil
		}
		return fmt.Errorf("create log partition %s: %w", name, err)
	}
	return nil
}

// ListLogPartitions returns the monthly children of log_lines, in
// ascending start order. The pre-cutover catch-all (log_lines_pre)
// is intentionally excluded — its bound is `MINVALUE` which doesn't
// fit the time-window vocabulary the sweeper uses for drop decisions.
// Operators rotate that one manually if needed.
func (s *Store) ListLogPartitions(ctx context.Context) ([]LogPartition, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.relname,
		       pg_get_expr(c.relpartbound, c.oid)
		FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'log_lines'::regclass
	`)
	if err != nil {
		return nil, fmt.Errorf("list log partitions: %w", err)
	}
	defer rows.Close()

	var out []LogPartition
	for rows.Next() {
		var name, bound string
		if err := rows.Scan(&name, &bound); err != nil {
			return nil, err
		}
		start, end, ok := parseRangeBound(bound)
		if !ok {
			// Catch-all (`MINVALUE`) and any other shape we don't
			// recognise — silently dropped from the lifecycle view.
			continue
		}
		out = append(out, LogPartition{Name: name, Start: start, End: end})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Start.Before(out[j].Start)
	})
	return out, nil
}

// DropLogPartition detaches and drops one monthly child. The detach
// step lets the parent stay scannable during the drop — on a busy
// log_lines this avoids briefly stalling reads on the parent OID
// while DROP TABLE acquires its lock.
func (s *Store) DropLogPartition(ctx context.Context, name string) error {
	if !validPartitionName.MatchString(name) {
		// Defence-in-depth: callers feed names from
		// ListLogPartitions, but a malformed name reaching DDL
		// would be the worst case to debug.
		return fmt.Errorf("refusing to drop partition with unsafe name %q", name)
	}
	if _, err := s.pool.Exec(ctx,
		fmt.Sprintf("ALTER TABLE log_lines DETACH PARTITION %s", name),
	); err != nil {
		return fmt.Errorf("detach log partition %s: %w", name, err)
	}
	if _, err := s.pool.Exec(ctx,
		fmt.Sprintf("DROP TABLE %s", name),
	); err != nil {
		return fmt.Errorf("drop log partition %s: %w", name, err)
	}
	return nil
}

// firstOfMonthUTC normalises any time inside a month to the first
// day at 00:00 UTC.
func firstOfMonthUTC(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// partitionBoundRange detects a normal monthly range (anything that
// isn't the MINVALUE catch-all). Captures lower and upper bounds.
var partitionBoundRange = regexp.MustCompile(
	`FOR VALUES FROM \('([^']+)'\) TO \('([^']+)'\)`,
)

// validPartitionName guards DDL built by string concatenation —
// only the names monthlyPartitionName produces are accepted.
var validPartitionName = regexp.MustCompile(`^log_lines_y\d{4}m\d{2}$`)

// parseRangeBound pulls the two timestamps out of expressions like
//
//	FOR VALUES FROM ('2026-04-01 00:00:00+00') TO ('2026-05-01 00:00:00+00')
//
// Postgres normalises the literal into its own preferred form, so
// we try a few layouts before giving up.
func parseRangeBound(expr string) (time.Time, time.Time, bool) {
	m := partitionBoundRange.FindStringSubmatch(expr)
	if len(m) != 3 {
		return time.Time{}, time.Time{}, false
	}
	lo, ok1 := parsePGTimestamp(m[1])
	hi, ok2 := parsePGTimestamp(m[2])
	return lo, hi, ok1 && ok2
}

func parsePGTimestamp(s string) (time.Time, bool) {
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05Z07",
		"2006-01-02 15:04:05.999999Z07",
		"2006-01-02 15:04:05-07",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
