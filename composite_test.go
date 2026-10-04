package gbase

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCompositeIndexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { db.Close() }()
	execTest(t, db, "CREATE TABLE items (id INTEGER PRIMARY KEY, cart INTEGER NOT NULL, product INTEGER NOT NULL, qty INTEGER, UNIQUE (cart, product))")
	execTest(t, db, "CREATE TABLE policies (id INTEGER PRIMARY KEY, insurer TEXT, num TEXT, expiry TEXT)")
	execTest(t, db, "INSERT INTO policies (insurer, num, expiry) VALUES ('A','1','2026-01-01'),('B','1','2026-02-01'),('A',NULL,'2026-03-01'),('A',NULL,'2026-04-01')")
	execTest(t, db, "CREATE UNIQUE INDEX insurer_num ON policies (insurer, num)")
	execTest(t, db, "CREATE INDEX insurer_expiry ON policies (insurer, expiry)")
	execTest(t, db, "INSERT INTO items (cart, product, qty) VALUES (1,10,1),(1,11,2),(2,10,3)")

	// Uniqueness covers the column tuple; NULL in any column exempts the row.
	if _, e := db.Exec(bg, "INSERT INTO items (cart, product) VALUES (1, 10)"); e == nil || !strings.Contains(e.Error(), "unique") {
		t.Fatalf("duplicate tuple accepted: %v", e)
	}
	if _, e := db.Exec(bg, "UPDATE items SET product = 11 WHERE id = 1"); e == nil {
		t.Fatal("duplicate tuple accepted by UPDATE")
	}
	execTest(t, db, "UPDATE items SET product = 12 WHERE id = 1")
	execTest(t, db, "INSERT INTO policies (insurer, num) VALUES ('A', NULL)")
	if _, e := db.Exec(bg, "INSERT INTO policies (insurer, num) VALUES ('B', '1')"); e == nil {
		t.Fatal("duplicate policy number accepted")
	}
	if _, e := db.Exec(bg, "CREATE UNIQUE INDEX dup ON policies (insurer, expiry, insurer)"); e == nil {
		t.Fatal("repeated index column accepted")
	}
	if _, e := db.Exec(bg, "CREATE UNIQUE INDEX bad ON policies (num, nope)"); e == nil {
		t.Fatal("unknown index column accepted")
	}

	plans := []struct{ sql, want string }{
		{"SELECT qty FROM items WHERE cart = 1 AND product = 11", "SEARCH items USING INDEX __gbase_items_cart_product (cart=? AND product=?)"},
		{"SELECT qty FROM items WHERE product = 11 AND 1 = cart", "SEARCH items USING INDEX __gbase_items_cart_product (cart=? AND product=?)"},
		{"SELECT qty FROM items WHERE cart = 1", "SEARCH items USING INDEX __gbase_items_cart_product (cart=?)"},
		{"SELECT qty FROM items WHERE product = 11", "SCAN items"},
		{"SELECT qty FROM items WHERE id = 1 AND cart = 1 AND product = 12", "SEARCH items USING INTEGER PRIMARY KEY (rowid=?)"},
		{"SELECT id FROM policies WHERE insurer = 'A' AND expiry = '2026-03-01'", "SEARCH policies USING INDEX insurer_expiry (insurer=? AND expiry=?)"},
	}
	for _, tt := range plans {
		if got := queryTest(t, db, "EXPLAIN QUERY PLAN "+tt.sql)[0][3]; got != tt.want {
			t.Errorf("%s:\n got %v\nwant %s", tt.sql, got, tt.want)
		}
	}
	results := []struct {
		sql  string
		want [][]Value
	}{
		{"SELECT qty FROM items WHERE cart = 1 AND product = 11", [][]Value{{int64(2)}}},
		{"SELECT product FROM items WHERE cart = 1 ORDER BY product", [][]Value{{int64(11)}, {int64(12)}}},
		{"SELECT id FROM policies WHERE insurer = 'A' AND expiry = '2026-03-01'", [][]Value{{int64(3)}}},
		{"SELECT id FROM policies WHERE insurer = 'A' AND expiry >= '2026-02-01' ORDER BY id", [][]Value{{int64(3)}, {int64(4)}}},
		{"SELECT id FROM policies WHERE insurer = 'A' AND num = '1'", [][]Value{{int64(1)}}},
	}
	for _, tt := range results {
		if got := queryTest(t, db, tt.sql); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: %v want %v", tt.sql, got, tt.want)
		}
	}

	// Composite indexes follow renames and block dropping their columns.
	execTest(t, db, "ALTER TABLE items RENAME COLUMN product TO sku")
	execTest(t, db, "ALTER TABLE items RENAME TO cart_items")
	if _, e := db.Exec(bg, "INSERT INTO cart_items (cart, sku) VALUES (2, 10)"); e == nil {
		t.Fatal("composite UNIQUE lost across renames")
	}
	if _, e := db.Exec(bg, "ALTER TABLE cart_items DROP COLUMN sku"); e == nil {
		t.Fatal("dropped a column of a composite UNIQUE")
	}
	if _, e := db.Exec(bg, "ALTER TABLE policies DROP COLUMN expiry"); e == nil {
		t.Fatal("dropped a column of a composite index")
	}
	if _, e := db.Exec(bg, "DROP INDEX __gbase_cart_items_cart_sku"); e == nil {
		t.Fatal("dropped a constraint index")
	}
	execTest(t, db, "DELETE FROM cart_items WHERE cart = 1")
	execTest(t, db, "DROP INDEX insurer_expiry")

	db.Close()
	if db, e = Open(path); e != nil {
		t.Fatal(e)
	}
	if e = db.Check(bg); e != nil {
		t.Fatal(e)
	}
	schema, e := db.Schema(bg)
	if e != nil {
		t.Fatal(e)
	}
	joined := strings.Join(schema, "\n")
	for _, want := range []string{`"qty" INTEGER, UNIQUE ("cart", "sku"));`, `CREATE UNIQUE INDEX "insurer_num" ON "policies"("insurer", "num");`} {
		if !strings.Contains(joined, want) {
			t.Errorf("schema missing %s:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "insurer_expiry") {
		t.Errorf("dropped index in schema:\n%s", joined)
	}
	// The rendered schema recreates an equivalent database.
	copyDB := openTest(t)
	for _, stmt := range schema {
		execTest(t, copyDB, stmt)
	}
	again, e := copyDB.Schema(bg)
	if e != nil || !reflect.DeepEqual(again, schema) {
		t.Fatalf("schema does not round-trip: %v\n%s", e, strings.Join(again, "\n"))
	}
}

func TestCompositeForeignKeyTargets(t *testing.T) {
	db := openTest(t)
	execTest(t, db, "CREATE TABLE p (id INTEGER PRIMARY KEY, a TEXT, b TEXT, UNIQUE (a, b))")
	// A composite UNIQUE does not make its leading column a valid single-column target.
	if _, e := db.Exec(bg, "CREATE TABLE c (id INTEGER PRIMARY KEY, a TEXT REFERENCES p(a))"); e == nil {
		t.Fatal("foreign key to a column that is only unique as part of a tuple")
	}
	for _, q := range []string{"CREATE TABLE t (a TEXT, UNIQUE (a, a))", "CREATE TABLE t (a TEXT, UNIQUE (a, nope))", "CREATE TABLE t (a TEXT, b TEXT, PRIMARY KEY (a, b))"} {
		if _, e := db.Exec(bg, q); e == nil {
			t.Errorf("%s accepted", q)
		}
	}
}
