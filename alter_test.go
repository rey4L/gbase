package gbase

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAlterTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { db.Close() }()
	execTest(t, db, "CREATE TABLE clients (id INTEGER PRIMARY KEY, name TEXT UNIQUE, legacy TEXT)")
	execTest(t, db, "CREATE TABLE policies (id INTEGER PRIMARY KEY, client INTEGER REFERENCES clients(id), num TEXT)")
	execTest(t, db, "CREATE INDEX policies_no ON policies(num)")
	execTest(t, db, "INSERT INTO clients (name, legacy) VALUES ('Ada','x'),('Bob','y')")
	execTest(t, db, "INSERT INTO policies (client, num) VALUES (1,'P1'),(2,'P2')")

	execTest(t, db, "ALTER TABLE clients ADD COLUMN status TEXT NOT NULL DEFAULT 'active'")
	execTest(t, db, "ALTER TABLE clients ADD phone TEXT")
	execTest(t, db, "ALTER TABLE policies ADD COLUMN broker INTEGER REFERENCES clients(id)")
	execTest(t, db, "ALTER TABLE clients DROP COLUMN legacy")
	execTest(t, db, "INSERT INTO clients (name, phone) VALUES ('Cy', '592')")
	if got, want := queryTest(t, db, "SELECT * FROM clients ORDER BY id"), [][]Value{{int64(1), "Ada", "active", nil}, {int64(2), "Bob", "active", nil}, {int64(3), "Cy", "active", "592"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after add/drop: %v", got)
	}

	execTest(t, db, "ALTER TABLE clients RENAME COLUMN name TO full_name")
	execTest(t, db, "ALTER TABLE clients RENAME TO customers")
	execTest(t, db, "ALTER TABLE policies RENAME num TO policy_no")
	if _, e := db.Exec(bg, "INSERT INTO customers (full_name) VALUES ('Ada')"); e == nil {
		t.Fatal("UNIQUE lost across renames")
	}
	if _, e := db.Exec(bg, "INSERT INTO policies (client) VALUES (99)"); e == nil {
		t.Fatal("foreign key lost across rename")
	}
	if _, e := db.Exec(bg, "DELETE FROM customers WHERE id = 1"); e == nil {
		t.Fatal("RESTRICT lost across rename")
	}
	plan := queryTest(t, db, "EXPLAIN QUERY PLAN SELECT id FROM policies WHERE policy_no = 'P2'")
	if plan[0][3] != "SEARCH policies USING INDEX policies_no (policy_no=?)" {
		t.Fatalf("index lost across column rename: %v", plan)
	}
	// The old name is free again, including its automatic index name.
	execTest(t, db, "CREATE TABLE clients (id INTEGER PRIMARY KEY, name TEXT UNIQUE)")

	db.Close()
	if db, e = Open(path); e != nil {
		t.Fatal(e)
	}
	if e = db.Check(bg); e != nil {
		t.Fatal(e)
	}
	if got := queryTest(t, db, "SELECT c.full_name, p.policy_no, p.broker FROM policies p JOIN customers c ON c.id = p.client ORDER BY p.id"); !reflect.DeepEqual(got, [][]Value{{"Ada", "P1", nil}, {"Bob", "P2", nil}}) {
		t.Fatalf("after reopen: %v", got)
	}
	schema, e := db.Schema(bg)
	if e != nil {
		t.Fatal(e)
	}
	joined := strings.Join(schema, "\n")
	for _, want := range []string{"customers", "full_name", `REFERENCES "customers"("id")`, "policy_no"} {
		if !strings.Contains(joined, want) {
			t.Errorf("schema missing %q:\n%s", want, joined)
		}
	}
}

func TestAlterTableRejects(t *testing.T) {
	db := openTest(t)
	execTest(t, db, "CREATE TABLE p (id INTEGER PRIMARY KEY, code TEXT UNIQUE, n INTEGER)")
	execTest(t, db, "CREATE TABLE c (id INTEGER PRIMARY KEY, p INTEGER REFERENCES p(id), code TEXT REFERENCES p(code))")
	execTest(t, db, "CREATE INDEX pn ON p(n)")
	execTest(t, db, "CREATE TABLE solo (only TEXT)")
	execTest(t, db, "INSERT INTO p (code, n) VALUES ('a', 1)")
	for _, q := range []string{
		"ALTER TABLE missing ADD x TEXT",
		"ALTER TABLE p ADD n TEXT",
		"ALTER TABLE p ADD x TEXT UNIQUE",
		"ALTER TABLE p ADD x INTEGER PRIMARY KEY",
		"ALTER TABLE p ADD x TEXT NOT NULL",
		"ALTER TABLE p ADD x INTEGER DEFAULT 'text'",
		"ALTER TABLE c ADD x INTEGER REFERENCES p(n)",
		"ALTER TABLE c ADD x INTEGER REFERENCES p(id) DEFAULT 1",
		"ALTER TABLE p DROP COLUMN id",
		"ALTER TABLE p DROP COLUMN code",
		"ALTER TABLE p DROP COLUMN n",
		"ALTER TABLE p DROP COLUMN nope",
		"ALTER TABLE solo DROP COLUMN only",
		"ALTER TABLE p RENAME TO c",
		"ALTER TABLE p RENAME TO pn",
		"ALTER TABLE p RENAME COLUMN n TO code",
		"ALTER TABLE p RENAME COLUMN nope TO x",
	} {
		if _, e := db.Exec(bg, q); e == nil {
			t.Errorf("%s accepted", q)
		}
	}
	// Failed statements leave the table untouched.
	if got := queryTest(t, db, "SELECT * FROM p"); !reflect.DeepEqual(got, [][]Value{{int64(1), "a", int64(1)}}) {
		t.Fatalf("table changed: %v", got)
	}
	if e := db.Check(bg); e != nil {
		t.Fatal(e)
	}
}

func TestAlterTableRollback(t *testing.T) {
	db := openTest(t)
	execTest(t, db, "CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT)")
	execTest(t, db, "INSERT INTO t (a) VALUES ('x')")
	tx, e := db.Begin(bg)
	if e != nil {
		t.Fatal(e)
	}
	for _, q := range []string{"ALTER TABLE t ADD b INTEGER DEFAULT 7", "ALTER TABLE t DROP a", "ALTER TABLE t RENAME TO u"} {
		if _, e = tx.Exec(bg, q); e != nil {
			t.Fatal(q, e)
		}
	}
	if e = tx.Rollback(); e != nil {
		t.Fatal(e)
	}
	if got := queryTest(t, db, "SELECT * FROM t"); !reflect.DeepEqual(got, [][]Value{{int64(1), "x"}}) {
		t.Fatalf("rollback: %v", got)
	}
}
