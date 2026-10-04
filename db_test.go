package gbase

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var bg = context.Background()

func openTest(t *testing.T) *DB {
	t.Helper()
	db, e := Open(filepath.Join(t.TempDir(), "test.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func execTest(t *testing.T, db *DB, q string, args ...any) Result {
	t.Helper()
	r, e := db.Exec(bg, q, args...)
	if e != nil {
		t.Fatalf("%s: %v", q, e)
	}
	return r
}

func queryTest(t *testing.T, db *DB, q string, args ...any) [][]Value {
	t.Helper()
	r, e := db.Query(bg, q, args...)
	if e != nil {
		t.Fatalf("%s: %v", q, e)
	}
	defer r.Close()
	var out [][]Value
	for r.Next() {
		out = append(out, r.Values())
	}
	if e = r.Err(); e != nil {
		t.Fatal(e)
	}
	return out
}

func TestCRUDConstraintsAndRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	execTest(t, db, "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL, age INTEGER DEFAULT 20, payload BLOB)")
	execTest(t, db, "CREATE TABLE notes (id INTEGER PRIMARY KEY, user_id INTEGER REFERENCES users(id), body TEXT)")
	execTest(t, db, "INSERT INTO users (name,payload) VALUES (?,?)", "Ada", []byte{0, 1, 255})
	execTest(t, db, "INSERT INTO users (id,name,age) VALUES (5,'Lin',30),(6,'Edsger',40)")
	execTest(t, db, "CREATE INDEX by_age ON users(age)")
	execTest(t, db, "INSERT INTO notes (user_id,body) VALUES (1,'hello')")
	for _, q := range []string{"INSERT INTO users (name) VALUES ('Ada')", "INSERT INTO users (name) VALUES (NULL)", "INSERT INTO notes (user_id) VALUES (99)", "DELETE FROM users WHERE id=1", "UPDATE users SET id=2 WHERE id=1", "UPDATE users SET age='bad'"} {
		if _, e = db.Exec(bg, q); e == nil {
			t.Fatalf("expected error: %s", q)
		}
	}
	execTest(t, db, "UPDATE users SET age=age+1 WHERE age>=30")
	got := queryTest(t, db, "SELECT name,age FROM users WHERE age>=30 ORDER BY age DESC")
	want := [][]Value{{"Edsger", int64(41)}, {"Lin", int64(31)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
	if e = db.Check(bg); e != nil {
		t.Fatal(e)
	}
	if e = db.Close(); e != nil {
		t.Fatal(e)
	}
	db, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	if got = queryTest(t, db, "SELECT name,age,payload FROM users WHERE id=1"); len(got) != 1 || got[0][1] != int64(20) || !reflect.DeepEqual(got[0][2], []byte{0, 1, 255}) {
		t.Fatalf("reopen: %#v", got)
	}
	if e = db.Check(bg); e != nil {
		t.Fatal(e)
	}
}

func TestStatementAndTransactionAtomicity(t *testing.T) {
	db := openTest(t)
	execTest(t, db, "CREATE TABLE t (id INTEGER PRIMARY KEY, n TEXT UNIQUE)")
	tx, e := db.Begin(bg)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if _, e = tx.Exec(bg, "INSERT INTO t VALUES (1,'a')"); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(bg, "INSERT INTO t VALUES (2,'b'),(3,'a')"); e == nil {
		t.Fatal("expected unique error")
	}
	r, e := tx.Query(bg, "SELECT id FROM t")
	if e != nil {
		t.Fatal(e)
	}
	if !r.Next() || r.Values()[0] != int64(1) || r.Next() {
		t.Fatal("statement was not rolled back")
	}
	r.Close()
	if _, e = tx.Exec(bg, "CREATE TABLE transient (id INTEGER)"); e != nil {
		t.Fatal(e)
	}
	if e = tx.Rollback(); e != nil {
		t.Fatal(e)
	}
	if len(queryTest(t, db, "SELECT * FROM t")) != 0 {
		t.Fatal("rollback failed")
	}
	if _, e = db.Query(bg, "SELECT * FROM transient"); e == nil {
		t.Fatal("DDL rollback failed")
	}
}

func TestJoinsGroupsAndNull(t *testing.T) {
	db := openTest(t)
	execTest(t, db, "CREATE TABLE teams (id INTEGER PRIMARY KEY, name TEXT)")
	execTest(t, db, "CREATE TABLE people (id INTEGER PRIMARY KEY, team INTEGER, score INTEGER)")
	execTest(t, db, "INSERT INTO teams VALUES (1,'a'),(2,'b'),(3,'c')")
	execTest(t, db, "INSERT INTO people VALUES (1,1,10),(2,1,20),(3,2,NULL)")
	got := queryTest(t, db, "SELECT t.name,COUNT(p.id),SUM(p.score) FROM teams t LEFT JOIN people p ON p.team=t.id GROUP BY t.name ORDER BY t.name")
	want := [][]Value{{"a", int64(2), int64(30)}, {"b", int64(1), nil}, {"c", int64(0), nil}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%#v", got)
	}
	got = queryTest(t, db, "SELECT team, AVG(score) AS avg_score FROM people GROUP BY team HAVING COUNT(*)>1 ORDER BY avg_score DESC")
	if len(got) != 1 || got[0][0] != int64(1) || got[0][1] != float64(15) {
		t.Fatalf("%#v", got)
	}
	got = queryTest(t, db, "SELECT NULL=1, NULL IS NULL, 0 AND NULL, 1 OR NULL")
	if !reflect.DeepEqual(got, [][]Value{{nil, int64(1), int64(0), int64(1)}}) {
		t.Fatalf("NULL: %#v", got)
	}
}

func TestLargeValuesAndReuse(t *testing.T) {
	db := openTest(t)
	execTest(t, db, "CREATE TABLE t (id INTEGER PRIMARY KEY, body TEXT)")
	body := strings.Repeat("hello", 6000)
	for i := 0; i < 30; i++ {
		execTest(t, db, "INSERT INTO t VALUES (?,?)", i, body)
	}
	if e := db.Check(bg); e != nil {
		t.Fatal(e)
	}
	execTest(t, db, "DELETE FROM t WHERE id>=10")
	execTest(t, db, "UPDATE t SET body='small' WHERE id<5")
	if e := db.Check(bg); e != nil {
		t.Fatal(e)
	}
	execTest(t, db, "DROP TABLE t")
	if e := db.Check(bg); e != nil {
		t.Fatal(e)
	}
}

func TestOwnershipAndCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	db, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if other, e := Open(path); e == nil {
		other.Close()
		t.Fatal("second opener accepted")
	}
	execTest(t, db, "CREATE TABLE t (id INTEGER)")
	rows, e := db.Query(bg, "SELECT * FROM t")
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(bg, 10*time.Millisecond)
	defer cancel()
	if _, e = db.Exec(ctx, "INSERT INTO t VALUES (1)"); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("%v", e)
	}
	rows.Close()
	execTest(t, db, "INSERT INTO t VALUES (1)")
	tx, e := db.Begin(bg)
	if e != nil {
		t.Fatal(e)
	}
	r, e := tx.Query(bg, "SELECT * FROM t")
	if e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); !errors.Is(e, ErrBusy) {
		t.Fatalf("%v", e)
	}
	r.Close()
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
}

func TestBindingAndTypes(t *testing.T) {
	db := openTest(t)
	execTest(t, db, "CREATE TABLE t (id INTEGER, r REAL)")
	for _, q := range []string{"SELECT missing FROM t", "SELECT t.missing FROM t", "SELECT * FROM nope"} {
		if _, e := db.Query(bg, q); e == nil {
			t.Fatalf("missing binding: %s", q)
		}
	}
	if _, e := db.Exec(bg, "INSERT INTO t VALUES (?,?)", 1); e == nil {
		t.Fatal("missing param")
	}
	execTest(t, db, "INSERT INTO t VALUES (?,?)", 1, 2)
	got := queryTest(t, db, "SELECT r FROM t")
	if got[0][0] != float64(2) {
		t.Fatalf("%#v", got)
	}
	if _, e := db.Exec(bg, "INSERT INTO t VALUES (?,?)", uint64(^uint64(0)), 1); e == nil {
		t.Fatal("overflow accepted")
	}
}

func TestConcurrentCallers(t *testing.T) {
	db := openTest(t)
	execTest(t, db, "CREATE TABLE t (id INTEGER PRIMARY KEY, n INTEGER)")
	done := make(chan error, 8)
	for worker := 0; worker < 8; worker++ {
		go func(w int) {
			for i := 0; i < 10; i++ {
				if _, e := db.Exec(bg, "INSERT INTO t (n) VALUES (?)", w*10+i); e != nil {
					done <- e
					return
				}
			}
			done <- nil
		}(worker)
	}
	for i := 0; i < 8; i++ {
		if e := <-done; e != nil {
			t.Fatal(e)
		}
	}
	if got := queryTest(t, db, "SELECT COUNT(*) FROM t"); got[0][0] != int64(80) {
		t.Fatal(got)
	}
	if e := db.Check(bg); e != nil {
		t.Fatal(e)
	}
}

func BenchmarkIndexedLookup(b *testing.B) {
	db, e := Open(filepath.Join(b.TempDir(), "b.db"))
	if e != nil {
		b.Fatal(e)
	}
	defer db.Close()
	db.Exec(bg, "CREATE TABLE t (id INTEGER PRIMARY KEY, n INTEGER)")
	tx, _ := db.Begin(bg)
	for i := 0; i < 1000; i++ {
		if _, e = tx.Exec(bg, "INSERT INTO t VALUES (?,?)", i, i); e != nil {
			b.Fatal(e)
		}
	}
	tx.Commit()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r, e := db.Query(bg, "SELECT n FROM t WHERE id=?", i%1000)
		if e != nil {
			b.Fatal(e)
		}
		for r.Next() {
			r.Values()
		}
		r.Close()
	}
}

func TestPrimaryDefaultsAndExhaustion(t *testing.T) {
	db := openTest(t)
	execTest(t, db, "CREATE TABLE t (id INTEGER PRIMARY KEY DEFAULT NULL, n TEXT)")
	execTest(t, db, "INSERT INTO t (n) VALUES ('a')")
	if got := queryTest(t, db, "SELECT id FROM t"); got[0][0] != int64(1) {
		t.Fatal(got)
	}
	execTest(t, db, "INSERT INTO t VALUES (?, 'max')", int64(9223372036854775807))
	if _, e := db.Exec(bg, "INSERT INTO t (n) VALUES ('overflow')"); e == nil {
		t.Fatal("rowid wrapped")
	}
	execTest(t, db, "INSERT INTO t VALUES (5,'explicit')")
	if e := db.Check(bg); e != nil {
		t.Fatal(e)
	}
	if _, e := db.Exec(bg, "CREATE TABLE bad (id TEXT PRIMARY KEY DEFAULT NULL)"); e == nil {
		t.Fatal("invalid default accepted")
	}
	queryTest(t, db, "SELECT * FROM t")
}

func TestDuplicateForeignKeysAndSchemaReplay(t *testing.T) {
	db := openTest(t)
	execTest(t, db, "CREATE TABLE z_parent (id INTEGER PRIMARY KEY)")
	execTest(t, db, "CREATE TABLE other (id INTEGER PRIMARY KEY)")
	if _, e := db.Exec(bg, "CREATE TABLE bad (id INTEGER REFERENCES z_parent(id), FOREIGN KEY(id) REFERENCES other(id))"); e == nil {
		t.Fatal("duplicate foreign keys overwritten")
	}
	execTest(t, db, "CREATE TABLE a_child (id INTEGER PRIMARY KEY, parent INTEGER REFERENCES z_parent(id))")
	schema, e := db.Schema(bg)
	if e != nil {
		t.Fatal(e)
	}
	copy := openTest(t)
	for _, s := range schema {
		execTest(t, copy, s)
	}
	if e = copy.Check(bg); e != nil {
		t.Fatal(e)
	}
}

func TestEmptyMutationBinding(t *testing.T) {
	db := openTest(t)
	execTest(t, db, "CREATE TABLE t (id INTEGER)")
	for _, q := range []string{"DELETE FROM t WHERE missing=1", "UPDATE t SET id=missing", "UPDATE t SET id=1 WHERE missing=2"} {
		if _, e := db.Exec(bg, q); e == nil {
			t.Fatalf("invalid binding accepted: %s", q)
		}
	}
}

func TestCloseAgainstQueriesAndInspection(t *testing.T) {
	for i := 0; i < 15; i++ {
		db := openTest(t)
		execTest(t, db, "CREATE TABLE t (id INTEGER)")
		done := make(chan struct{})
		go func() {
			defer close(done)
			rows, e := db.Query(bg, "SELECT * FROM t")
			if e == nil {
				for rows.Next() {
					rows.Values()
				}
				rows.Close()
			}
		}()
		db.Close()
		<-done
	}
	for i := 0; i < 15; i++ {
		db := openTest(t)
		execTest(t, db, "CREATE TABLE t (id INTEGER)")
		done := make(chan struct{})
		go func() { defer close(done); db.Check(bg); db.Schema(bg); db.Tables(bg) }()
		db.Close()
		<-done
	}
}

func TestForeignKeyIndexesAndSelfReferences(t *testing.T) {
	db := openTest(t)
	execTest(t, db, "CREATE TABLE z_parent (id INTEGER PRIMARY KEY, name TEXT)")
	execTest(t, db, "CREATE UNIQUE INDEX parent_name ON z_parent(name)")
	execTest(t, db, "CREATE TABLE a_child (id INTEGER PRIMARY KEY, name TEXT REFERENCES z_parent(name))")
	execTest(t, db, "INSERT INTO z_parent VALUES (1,'x')")
	execTest(t, db, "INSERT INTO a_child VALUES (1,'x')")
	if _, e := db.Exec(bg, "DROP INDEX parent_name"); e == nil {
		t.Fatal("foreign backing index dropped")
	}
	if e := db.Check(bg); e != nil {
		t.Fatal(e)
	}
	schema, e := db.Schema(bg)
	if e != nil {
		t.Fatal(e)
	}
	copy := openTest(t)
	for _, s := range schema {
		execTest(t, copy, s)
	}
	execTest(t, db, "CREATE TABLE nodes (id INTEGER PRIMARY KEY, parent INTEGER REFERENCES nodes(id))")
	execTest(t, db, "INSERT INTO nodes VALUES (1,2),(2,NULL)")
	if e = db.Check(bg); e != nil {
		t.Fatal(e)
	}
}

func TestCursorCancellation(t *testing.T) {
	db := openTest(t)
	execTest(t, db, "CREATE TABLE t (id INTEGER)")
	execTest(t, db, "INSERT INTO t VALUES (1),(2)")
	ctx, cancel := context.WithCancel(bg)
	r, e := db.Query(ctx, "SELECT * FROM t")
	if e != nil {
		t.Fatal(e)
	}
	if !r.Next() {
		t.Fatal(r.Err())
	}
	cancel()
	if r.Next() || !errors.Is(r.Err(), context.Canceled) {
		t.Fatalf("cursor cancellation: %v", r.Err())
	}
	execTest(t, db, "INSERT INTO t VALUES (3)")
	if tx, e := db.Begin(ctx); e == nil {
		tx.Rollback()
		t.Fatal("canceled Begin succeeded")
	}
}

func TestUpdateWritesOnlyChangedPages(t *testing.T) {
	db := openTest(t)
	execTest(t, db, "CREATE TABLE t (id INTEGER PRIMARY KEY, n INTEGER)")
	execTest(t, db, "INSERT INTO t VALUES (1,1)")
	writes := 0
	db.pager.Fault = func(point string) error {
		if point == "db-page-written" {
			writes++
		}
		return nil
	}
	execTest(t, db, "UPDATE t SET n=2 WHERE id=1")
	db.pager.Fault = nil
	if writes != 2 {
		t.Fatalf("expected table page and metadata page, wrote %d pages", writes)
	}
	if got := queryTest(t, db, "SELECT n FROM t"); got[0][0] != int64(2) {
		t.Fatal(got)
	}
}

func TestBusyTimeout(t *testing.T) {
	db := openTest(t)
	tx, e := db.Begin(bg)
	if e != nil {
		t.Fatal(e)
	}
	db.SetBusyTimeout(20 * time.Millisecond)
	start := time.Now()
	if _, e = db.Exec(bg, "CREATE TABLE t (id INTEGER PRIMARY KEY)"); !errors.Is(e, ErrLocked) {
		t.Fatalf("got %v, want ErrLocked", e)
	}
	if waited := time.Since(start); waited < 20*time.Millisecond || waited > time.Second {
		t.Fatalf("waited %v", waited)
	}
	done := make(chan error)
	db.SetBusyTimeout(5 * time.Second)
	go func() { _, e := db.Exec(bg, "CREATE TABLE t (id INTEGER PRIMARY KEY)"); done <- e }()
	time.Sleep(10 * time.Millisecond)
	tx.Rollback()
	if e = <-done; e != nil {
		t.Fatalf("waiter after release: %v", e)
	}
}
