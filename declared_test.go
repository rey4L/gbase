package gbase

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDeclaredTypes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { db.Close() }()
	execTest(t, db, "CREATE TABLE p (id INTEGER PRIMARY KEY, active BOOLEAN NOT NULL DEFAULT 1, starts DATE, created TIMESTAMP DEFAULT '2026-01-01', seen DATETIME)")
	execTest(t, db, "CREATE INDEX p_created ON p(created)")
	inserts := []struct {
		starts, created string
	}{
		{"2026-03-01", "2026-03-01T10:00:00.5-04:00"},
		{"2026-01-15T00:00:00Z", "2026-03-01 14:00:00"},
		{"2026-02-01", "2026-03-01T14:00:00.123456789Z"},
	}
	for _, in := range inserts {
		execTest(t, db, "INSERT INTO p (starts, created) VALUES (?, ?)", in.starts, in.created)
	}
	execTest(t, db, "INSERT INTO p (active, seen) VALUES (0, ?)", FormatTimestamp(time.Date(2026, 5, 6, 7, 8, 9, 0, time.FixedZone("GYT", -4*3600))))
	got := queryTest(t, db, "SELECT id, active, starts, created, seen FROM p ORDER BY created, id")
	// Fixed-width fractions make text order time order: .000000 < .123456 < .500000.
	want := [][]Value{
		{int64(4), int64(0), nil, "2026-01-01T00:00:00.000000Z", "2026-05-06T11:08:09.000000Z"},
		{int64(2), int64(1), "2026-01-15", "2026-03-01T14:00:00.000000Z", nil},
		{int64(3), int64(1), "2026-02-01", "2026-03-01T14:00:00.123456Z", nil},
		{int64(1), int64(1), "2026-03-01", "2026-03-01T14:00:00.500000Z", nil},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	if got := queryTest(t, db, "SELECT id FROM p WHERE created >= ? AND created < ? ORDER BY id", "2026-03-01T14:00:00.200000Z", "2026-03-02"); !reflect.DeepEqual(got, [][]Value{{int64(1)}}) {
		t.Fatalf("range on canonical text: %v", got)
	}
	for _, q := range []string{
		"INSERT INTO p (active) VALUES (2)",
		"INSERT INTO p (active) VALUES ('yes')",
		"INSERT INTO p (starts) VALUES ('2026-1-5')",
		"INSERT INTO p (starts) VALUES ('2026-02-30')",
		"INSERT INTO p (starts) VALUES ('2026-01-15T04:00:00Z')",
		"INSERT INTO p (created) VALUES ('yesterday')",
		"INSERT INTO p (created) VALUES (1767225600)",
		"UPDATE p SET seen = 'soon'",
		"CREATE TABLE b (flag BOOLEAN PRIMARY KEY)",
		"CREATE TABLE b (flag BOOLEAN DEFAULT 5)",
	} {
		if _, e := db.Exec(bg, q); e == nil {
			t.Errorf("%s accepted", q)
		}
	}
	execTest(t, db, "ALTER TABLE p ADD COLUMN closed DATE DEFAULT '2027-01-01'")

	r, e := db.Query(bg, "SELECT p.*, created || '' AS label, 1 AS one FROM p WHERE id = 1")
	if e != nil {
		t.Fatal(e)
	}
	types := r.DeclTypes()
	r.Close()
	if want := []string{"INTEGER", "BOOLEAN", "DATE", "TIMESTAMP", "DATETIME", "DATE", "", ""}; !reflect.DeepEqual(types, want) {
		t.Fatalf("DeclTypes %q want %q", types, want)
	}

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
	want0 := `CREATE TABLE "p" ("id" INTEGER PRIMARY KEY, "active" BOOLEAN NOT NULL DEFAULT 1, "starts" DATE, "created" TIMESTAMP DEFAULT '2026-01-01T00:00:00.000000Z', "seen" DATETIME, "closed" DATE DEFAULT '2027-01-01');`
	if !strings.Contains(joined, want0) {
		t.Fatalf("schema:\n%s", joined)
	}
	copyDB := openTest(t)
	for _, stmt := range schema {
		execTest(t, copyDB, stmt)
	}
	if again, _ := copyDB.Schema(bg); !reflect.DeepEqual(again, schema) {
		t.Fatalf("schema does not round-trip:\n%s", strings.Join(again, "\n"))
	}
}
