package sqldriver

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rey4L/gbase"
)

var bg = context.Background()

func open(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, e := sql.Open("gbase", dsn)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func exec(t *testing.T, db interface {
	Exec(string, ...any) (sql.Result, error)
}, q string, args ...any) sql.Result {
	t.Helper()
	r, e := db.Exec(q, args...)
	if e != nil {
		t.Fatalf("%s: %v", q, e)
	}
	return r
}

func TestDatabaseSQL(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "app.db"))
	exec(t, db, `
		CREATE TABLE clients (id INTEGER PRIMARY KEY, name TEXT NOT NULL, active BOOLEAN NOT NULL DEFAULT 1, since DATE, seen TIMESTAMP, photo BLOB, score REAL);
		-- a comment; with a semicolon
		CREATE INDEX clients_name ON clients (name);`)
	r := exec(t, db, "INSERT INTO clients (name, active, since, seen, photo, score) VALUES (?, ?, ?, ?, ?, ?)",
		"Ada; Lovelace", false, "2026-01-15", time.Date(2026, 3, 1, 10, 0, 0, 123456789, time.FixedZone("GYT", -4*3600)), []byte{0, 1}, 2.5)
	if id, _ := r.LastInsertId(); id != 1 {
		t.Fatalf("LastInsertId %d", id)
	}
	exec(t, db, "INSERT INTO clients (name) VALUES (:name), (@name || '2')", sql.Named("name", "Bob"))

	var (
		name   string
		active bool
		since  time.Time
		seen   sql.NullTime
		photo  []byte
		score  sql.NullFloat64
	)
	row := db.QueryRow("SELECT name, active, since, seen, photo, score FROM clients WHERE id = $id", sql.Named("id", 1))
	if e := row.Scan(&name, &active, &since, &seen, &photo, &score); e != nil {
		t.Fatal(e)
	}
	wantSeen := time.Date(2026, 3, 1, 14, 0, 0, 123456000, time.UTC)
	if name != "Ada; Lovelace" || active || !since.Equal(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)) || !seen.Time.Equal(wantSeen) || !reflect.DeepEqual(photo, []byte{0, 1}) || score.Float64 != 2.5 {
		t.Fatalf("%q %v %v %v %v %v", name, active, since, seen, photo, score)
	}
	// NULLs scan into nullable types; a time.Time argument matches the stored form.
	if e := db.QueryRow("SELECT seen, score FROM clients WHERE id = 2").Scan(&seen, &score); e != nil || seen.Valid || score.Valid {
		t.Fatalf("NULL scan: %v %v %v", e, seen, score)
	}
	var id int64
	if e := db.QueryRow("SELECT id FROM clients WHERE seen = ?", wantSeen).Scan(&id); e != nil || id != 1 {
		t.Fatalf("time argument lookup: %v %d", e, id)
	}

	rows, e := db.Query("SELECT id, active, since, name || '' AS label FROM clients ORDER BY id")
	if e != nil {
		t.Fatal(e)
	}
	types, _ := rows.ColumnTypes()
	var names []string
	for _, ct := range types {
		names = append(names, ct.DatabaseTypeName()+":"+ct.ScanType().String())
	}
	if want := []string{"INTEGER:sql.NullInt64", "BOOLEAN:sql.NullBool", "DATE:sql.NullTime", ":interface {}"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("column types %q", names)
	}
	n := 0
	for rows.Next() {
		n++
		// Results are buffered, so other statements run while iterating.
		exec(t, db, "UPDATE clients SET score = ? WHERE id = ?", n, n)
	}
	if e = rows.Err(); e != nil || n != 3 {
		t.Fatal(e, n)
	}
	rows.Close()

	stmt, e := db.Prepare("SELECT name FROM clients WHERE id = ?")
	if e != nil {
		t.Fatal(e)
	}
	defer stmt.Close()
	for i, want := range []string{"Ada; Lovelace", "Bob", "Bob2"} {
		var got string
		if e = stmt.QueryRow(i + 1).Scan(&got); e != nil || got != want {
			t.Fatalf("prepared %d: %v %q", i, e, got)
		}
	}
	if _, e = db.Prepare("SELEC 1"); e == nil {
		t.Fatal("Prepare accepted a syntax error")
	}

	for _, tt := range []struct {
		q    string
		args []any
	}{
		{"SELECT ?", nil},
		{"SELECT ?", []any{1, 2}},
		{"SELECT :a", []any{sql.Named("b", 1)}},
		{"SELECT :a", []any{sql.Named("a", 1), sql.Named("b", 2)}},
		{"SELECT :a, :b", []any{sql.Named("a", 1), 2}},
		{"SELECT 1; SELECT 2", nil},
		{"INSERT INTO clients (name) VALUES (?)", []any{struct{}{}}},
	} {
		if _, e := db.Query(tt.q, tt.args...); e == nil {
			t.Errorf("%s %v accepted", tt.q, tt.args)
		}
	}
}

func TestTransactions(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "app.db"))
	exec(t, db, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT UNIQUE)")

	tx, e := db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	exec(t, tx, "INSERT INTO t (v) VALUES ('a')")
	rows, e := tx.Query("SELECT v FROM t")
	if e != nil {
		t.Fatal(e)
	}
	for rows.Next() {
		exec(t, tx, "INSERT INTO t (v) VALUES ('b')")
	}
	rows.Close()
	if e = tx.Rollback(); e != nil {
		t.Fatal(e)
	}
	var n int
	if db.QueryRow("SELECT COUNT(*) FROM t").Scan(&n); n != 0 {
		t.Fatalf("rollback kept %d rows", n)
	}

	// A failing statement in a multi-statement Exec applies none of them.
	if _, e = db.Exec("INSERT INTO t (v) VALUES ('x'); INSERT INTO t (v) VALUES ('x')"); e == nil {
		t.Fatal("duplicate accepted")
	}
	if db.QueryRow("SELECT COUNT(*) FROM t").Scan(&n); n != 0 {
		t.Fatalf("partial multi-statement Exec kept %d rows", n)
	}

	ro, e := db.BeginTx(bg, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = ro.Exec("INSERT INTO t (v) VALUES ('r')"); e == nil {
		t.Fatal("read-only transaction wrote")
	}
	if e = ro.QueryRow("SELECT COUNT(*) FROM t").Scan(&n); e != nil {
		t.Fatal(e)
	}
	ro.Rollback()

	tx, _ = db.Begin()
	exec(t, tx, "INSERT INTO t (v) VALUES (?)", "c")
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	if db.QueryRow("SELECT COUNT(*) FROM t").Scan(&n); n != 1 {
		t.Fatalf("commit: %d rows", n)
	}
}

func TestBusyTimeoutAndSharing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	db := open(t, path+"?busy_timeout=50ms")
	exec(t, db, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	tx, e := db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	// Another pooled connection waits for the open transaction, then gives up.
	start := time.Now()
	if _, e = db.Exec("INSERT INTO t VALUES (1)"); !errors.Is(e, gbase.ErrLocked) {
		t.Fatalf("got %v, want ErrLocked", e)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Fatal("did not wait")
	}
	tx.Rollback()

	// A second sql.DB on the same file shares the open database.
	other := open(t, "file:"+path+"?_busy_timeout=50")
	exec(t, other, "INSERT INTO t VALUES (2)")
	var n int
	if db.QueryRow("SELECT COUNT(*) FROM t").Scan(&n); n != 1 {
		t.Fatalf("shared file: %d rows", n)
	}
	if _, e = open(t, path+"?busy_timeout=1s").Exec("SELECT 1"); e == nil || !strings.Contains(e.Error(), "busy_timeout") {
		t.Fatalf("conflicting busy_timeout: %v", e)
	}
	for _, dsn := range []string{"", "?busy_timeout=1s", path + "?busy_timeout=-1", path + "?cache=shared"} {
		bad, e := sql.Open("gbase", dsn)
		if e == nil {
			_, e = bad.Exec("SELECT 1")
			bad.Close()
		}
		if e == nil {
			t.Errorf("DSN %q accepted", dsn)
		}
	}

	// The file is released when every connection closes, and can be reopened.
	db.Close()
	other.Close()
	if g, e := gbase.Open(path); e != nil {
		t.Fatalf("file still held: %v", e)
	} else {
		g.Close()
	}
}

func TestRawBackupAndConcurrency(t *testing.T) {
	dir := t.TempDir()
	db := open(t, filepath.Join(dir, "app.db"))
	exec(t, db, "CREATE TABLE t (id INTEGER PRIMARY KEY, worker INTEGER)")
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if _, e := db.Exec("INSERT INTO t (worker) VALUES (?)", w); e != nil {
					t.Error(e)
					return
				}
				var n int
				if e := db.QueryRow("SELECT COUNT(*) FROM t WHERE worker = ?", w).Scan(&n); e != nil || n != i+1 {
					t.Errorf("worker %d: %v %d", w, e, n)
					return
				}
			}
		}()
	}
	wg.Wait()

	conn, e := db.Conn(bg)
	if e != nil {
		t.Fatal(e)
	}
	backup := filepath.Join(dir, "backup.db")
	if e = conn.Raw(func(c any) error { return c.(*Conn).DB().BackupFile(bg, backup) }); e != nil {
		t.Fatal(e)
	}
	conn.Close()
	copyDB := open(t, backup)
	var n int
	if e = copyDB.QueryRow("SELECT COUNT(*) FROM t").Scan(&n); e != nil || n != 200 {
		t.Fatalf("backup: %v %d", e, n)
	}
}
