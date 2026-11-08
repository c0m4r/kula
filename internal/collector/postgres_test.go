package collector

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// pgFakeServer answers the queries of a PostgreSQL poll with fixed rows and
// counts how often each one runs.
type pgFakeServer struct {
	mu      sync.Mutex
	queries map[string]int
}

// pgFakeAnswers maps a fragment of each query, checked in order, to the name
// it is counted under and its one row.
var pgFakeAnswers = []struct {
	fragment, name string
	row            []driver.Value
}{
	{"pg_backend_pid()", "session", []driver.Value{int64(4242), "2026-01-01 00:00:00+00", int64(170000)}},
	{"FROM pg_stat_activity", "activity", []driver.Value{int64(1), int64(2), int64(0), int64(0)}},
	{"SHOW max_connections", "max_connections", []driver.Value{"100"}},
	{"FROM pg_stat_database", "database", []driver.Value{int64(10), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)}},
	{"pg_stat_checkpointer", "bgwriter", []driver.Value{int64(0), int64(0)}},
	{"FROM pg_stat_user_tables", "user_tables", []driver.Value{int64(7), int64(700), int64(3)}},
	{"pg_database_size", "database_size", []driver.Value{int64(8_000_000)}},
	{"pg_is_in_recovery", "recovery", []driver.Value{false}},
	{"pg_stat_replication", "replication", []driver.Value{int64(0)}},
}

func (s *pgFakeServer) Connect(context.Context) (driver.Conn, error) { return &pgFakeConn{s}, nil }
func (s *pgFakeServer) Driver() driver.Driver                        { return fakeDBDriver{} }

func (s *pgFakeServer) count(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queries[name]
}

type pgFakeConn struct{ srv *pgFakeServer }

func (c *pgFakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (c *pgFakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("not supported") }
func (c *pgFakeConn) Close() error                        { return nil }

func (c *pgFakeConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(0), nil // the session's SET statements
}

func (c *pgFakeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	for _, a := range pgFakeAnswers {
		if strings.Contains(query, a.fragment) {
			c.srv.mu.Lock()
			c.srv.queries[a.name]++
			c.srv.mu.Unlock()
			return &pgFakeRows{row: a.row}, nil
		}
	}
	return nil, fmt.Errorf("unexpected query %q", query)
}

type pgFakeRows struct {
	row  []driver.Value
	done bool
}

func (r *pgFakeRows) Columns() []string { return make([]string, len(r.row)) }
func (r *pgFakeRows) Close() error      { return nil }

func (r *pgFakeRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	copy(dest, r.row)
	r.done = true
	return nil
}

// Table health and the database size walk every relation on the server, so
// a poll every second reads them once a minute, and every poll in between
// reports what the last read found. Everything else is still read each poll.
func TestPostgresRelationStatsOncePerInterval(t *testing.T) {
	srv := &pgFakeServer{queries: map[string]int{}}
	clock := time.Unix(1_700_000_000, 0)
	pc := newPostgresCollector("localhost", 5432, "kula", "", "app", "disable", false, time.Second)
	pc.session.connector = srv
	pc.session.now = func() time.Time { return clock }
	pc.session.logf = sessionLog(t)
	defer pc.session.release()

	const polls = 121 // two minutes: reads at 0, 60 and 120 s
	for poll := 0; poll < polls; poll++ {
		stats := pc.poll(context.Background())
		if stats == nil {
			t.Fatalf("poll %d failed", poll)
		}
		if stats.DBSizeBytes != 8_000_000 || stats.DeadTuples != 7 || stats.LiveTuples != 700 || stats.AutovacuumCount != 3 {
			t.Fatalf("poll %d: size %d, tuples %d dead / %d live, %d autovacuums: want the last read repeated",
				poll, stats.DBSizeBytes, stats.DeadTuples, stats.LiveTuples, stats.AutovacuumCount)
		}
		// Polls drift a little early; none may push a read to the next poll.
		clock = clock.Add(time.Second - time.Millisecond)
	}
	for name, want := range map[string]int{"user_tables": 3, "database_size": 3, "database": polls, "activity": polls} {
		if got := srv.count(name); got != want {
			t.Errorf("%s queried %d times in %d polls, want %d", name, got, polls, want)
		}
	}
}

// At a 30 s collection interval the relation statistics are read every
// second poll, and never more often than each poll.
func TestPostgresRelationStatsDue(t *testing.T) {
	pc := &postgresCollector{session: dbSession{timeouts: newDBTimeouts(30 * time.Second)}}
	reads := 0
	for poll := 0; poll < 6; poll++ {
		at := time.Unix(1_700_000_000, 0).Add(time.Duration(poll) * 30 * time.Second)
		if pc.relationStatsDue(at) {
			reads++
			pc.relAt = at
		}
	}
	if reads != 3 {
		t.Fatalf("%d reads in 6 polls 30 s apart, want 3", reads)
	}
}
