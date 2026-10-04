package gbase

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestPrimaryKeyAccessPaths(t *testing.T) {
	db := openTest(t)
	execTest(t, db, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	execTest(t, db, "INSERT INTO t VALUES (?),(?),(?),(?),(?)", int64(math.MinInt64), -1, 0, 1, int64(math.MaxInt64))
	plan := queryTest(t, db, "EXPLAIN SELECT id FROM t WHERE id=?", 1)
	if plan[0][1] != "IndexSeek" || !strings.Contains(plan[0][2].(string), "PRIMARY KEY") {
		t.Fatal(plan)
	}
	tests := []struct {
		sql  string
		arg  any
		want []int64
	}{{"id = ?", int64(math.MaxInt64), []int64{math.MaxInt64}}, {"id > ?", int64(math.MaxInt64), nil}, {"id < ?", int64(math.MinInt64), nil}, {"id <= ?", int64(math.MinInt64), []int64{math.MinInt64}}, {"id >= ?", int64(math.MaxInt64), []int64{math.MaxInt64}}, {"id > ?", 0, []int64{1, math.MaxInt64}}, {"? < id", 0, []int64{1, math.MaxInt64}}, {"id = ?", float64(1), []int64{1}}}
	for _, tt := range tests {
		got := queryTest(t, db, "SELECT id FROM t WHERE "+tt.sql+" ORDER BY id", tt.arg)
		var ids []int64
		for _, r := range got {
			ids = append(ids, r[0].(int64))
		}
		if !reflect.DeepEqual(ids, tt.want) {
			t.Fatalf("%s %v: %v want %v", tt.sql, tt.arg, ids, tt.want)
		}
	}
}

func TestIndexScanEquivalence(t *testing.T) {
	db := openTest(t)
	execTest(t, db, "CREATE TABLE t (id INTEGER PRIMARY KEY, n INTEGER, r REAL, s TEXT)")
	execTest(t, db, "INSERT INTO t VALUES (1,-2,-2.0,''),(2,0,0.0,'a'),(3,1,1.0,'a'),(4,2,2.0,'b'),(5,NULL,NULL,NULL)")
	queries := []string{"SELECT id FROM t WHERE n<1 ORDER BY id", "SELECT id FROM t WHERE n<=1 ORDER BY id", "SELECT id FROM t WHERE n>1 ORDER BY id", "SELECT id FROM t WHERE n>=1 ORDER BY id", "SELECT id FROM t WHERE 1<n ORDER BY id", "SELECT id FROM t WHERE n=1.0 ORDER BY id", "SELECT id FROM t WHERE r=1 ORDER BY id", "SELECT id FROM t WHERE r>=0.0 ORDER BY id", "SELECT id FROM t WHERE s='a' ORDER BY id", "SELECT id FROM t WHERE s>'a' ORDER BY id", "SELECT id FROM t WHERE s IS NULL ORDER BY id", "SELECT id FROM t WHERE n=1 OR n=-2 ORDER BY id"}
	expected := make([][][]Value, len(queries))
	for i, q := range queries {
		expected[i] = queryTest(t, db, q)
	}
	execTest(t, db, "CREATE INDEX n_idx ON t(n)")
	execTest(t, db, "CREATE INDEX r_idx ON t(r)")
	execTest(t, db, "CREATE INDEX s_idx ON t(s)")
	for i, q := range queries {
		if got := queryTest(t, db, q); !reflect.DeepEqual(got, expected[i]) {
			t.Fatalf("%s: %#v want %#v", q, got, expected[i])
		}
	}
	plan := queryTest(t, db, "EXPLAIN SELECT id FROM t WHERE s='a'")
	if plan[0][1] != "IndexSeek" {
		t.Fatal(plan)
	}
}

func TestQueryOperatorsAndFunctions(t *testing.T) {
	db := openTest(t)
	execTest(t, db, "CREATE TABLE t (id INTEGER PRIMARY KEY, n INTEGER, name TEXT)")
	execTest(t, db, "INSERT INTO t VALUES (1,10,'Ada'),(2,20,'Lin'),(3,20,'Ada'),(4,NULL,NULL)")
	cases := []struct {
		query string
		want  [][]Value
	}{{"SELECT DISTINCT name FROM t ORDER BY name", [][]Value{{nil}, {"Ada"}, {"Lin"}}}, {"SELECT COUNT(*),COUNT(n),SUM(n),AVG(n),MIN(n),MAX(n) FROM t", [][]Value{{int64(4), int64(3), int64(50), float64(50) / 3, int64(10), int64(20)}}}, {"SELECT id AS x,name FROM t ORDER BY 1 DESC LIMIT 2 OFFSET 1", [][]Value{{int64(3), "Ada"}, {int64(2), "Lin"}}}, {"SELECT LOWER(name),UPPER(name),LENGTH(name),ABS(-4),COALESCE(NULL,name) FROM t WHERE id=1", [][]Value{{"ada", "ADA", int64(3), int64(4), "Ada"}}}, {"SELECT t.id,u.id FROM t INNER JOIN t u ON t.id=u.id WHERE t.id<=2 ORDER BY t.id", [][]Value{{int64(1), int64(1)}, {int64(2), int64(2)}}}, {"SELECT NULL+1,5/2,2.5*2,NOT NULL", [][]Value{{nil, int64(2), float64(5), nil}}}}
	for _, tt := range cases {
		if got := queryTest(t, db, tt.query); !reflect.DeepEqual(got, tt.want) {
			t.Fatalf("%s: %#v want %#v", tt.query, got, tt.want)
		}
	}
}

func TestInvalidQueriesBindBeforeRows(t *testing.T) {
	db := openTest(t)
	execTest(t, db, "CREATE TABLE t (id INTEGER)")
	for _, q := range []string{"SELECT id FROM t a INNER JOIN t b ON a.id=b.id", "SELECT SUM(id),id FROM t", "SELECT COUNT(SUM(id)) FROM t", "SELECT id FROM t WHERE COUNT(*)>1", "SELECT id FROM t GROUP BY COUNT(*)", "SELECT id FROM t HAVING id>1", "SELECT DOES_NOT_EXIST(id) FROM t", "SELECT LOWER(id,id) FROM t", "SELECT id FROM t ORDER BY missing", "SELECT id FROM t LIMIT -1"} {
		if rows, e := db.Query(bg, q); e == nil {
			rows.Close()
			t.Fatalf("invalid query accepted: %s", q)
		}
	}
}

func TestStreamingAndOperationCancellation(t *testing.T) {
	db := openTest(t)
	execTest(t, db, "CREATE TABLE t (id INTEGER)")
	execTest(t, db, "INSERT INTO t VALUES (0),(1)")
	rows, e := db.Query(bg, "SELECT 10/id FROM t")
	if e != nil {
		t.Fatalf("simple query evaluated eagerly: %v", e)
	}
	if rows.Next() || rows.Err() == nil {
		t.Fatal("streaming arithmetic error lost")
	}
	rows.Close()
	execTest(t, db, "INSERT INTO t VALUES (2)")
	tx, e := db.Begin(bg)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	ctx, cancel := context.WithCancel(bg)
	r, e := tx.Query(ctx, "SELECT * FROM t WHERE id<0")
	if e != nil {
		t.Fatal(e)
	}
	cancel()
	if r.Next() || !errors.Is(r.Err(), context.Canceled) {
		t.Fatal(r.Err())
	}
	if _, e = tx.Exec(bg, "INSERT INTO t VALUES (3)"); e != nil {
		t.Fatal(e)
	}
}

func TestExplainQueryPlan(t *testing.T) {
	db := openTest(t)
	execTest(t, db, "CREATE TABLE t (id INTEGER PRIMARY KEY, a INTEGER, b TEXT)")
	execTest(t, db, "CREATE TABLE u (id INTEGER PRIMARY KEY, c INTEGER)")
	execTest(t, db, "CREATE INDEX ia ON t(a)")
	tests := []struct {
		sql  string
		want []string
	}{
		{"SELECT 1", []string{"SCAN CONSTANT ROW"}},
		{"SELECT * FROM t", []string{"SCAN t"}},
		{"SELECT * FROM t AS x", []string{"SCAN x"}},
		{"SELECT * FROM t WHERE id = ?", []string{"SEARCH t USING INTEGER PRIMARY KEY (rowid=?)"}},
		{"SELECT * FROM t WHERE id >= 3", []string{"SEARCH t USING INTEGER PRIMARY KEY (rowid>?)"}},
		{"SELECT * FROM t WHERE 3 > id", []string{"SEARCH t USING INTEGER PRIMARY KEY (rowid<?)"}},
		{"SELECT * FROM t WHERE a = 1", []string{"SEARCH t USING INDEX ia (a=?)"}},
		{"SELECT * FROM t WHERE a = 1 OR a = 2", []string{"SCAN t"}},
		{"SELECT * FROM t JOIN u ON t.id = u.id", []string{"SCAN t", "SCAN u"}},
		{"SELECT b, COUNT(*) FROM t GROUP BY b", []string{"SCAN t", "USE TEMP B-TREE FOR GROUP BY"}},
		{"SELECT COUNT(*) FROM t", []string{"SCAN t"}},
		{"SELECT DISTINCT b FROM t ORDER BY b", []string{"SCAN t", "USE TEMP B-TREE FOR DISTINCT", "USE TEMP B-TREE FOR ORDER BY"}},
		{"UPDATE t SET a = 1 WHERE id = 1", []string{"SCAN t"}},
		{"DELETE FROM t WHERE a = 1", []string{"SCAN t"}},
	}
	for _, tt := range tests {
		var args []any
		if strings.Contains(tt.sql, "?") {
			args = append(args, 1)
		}
		rows := queryTest(t, db, "EXPLAIN QUERY PLAN "+tt.sql, args...)
		var got []string
		for _, r := range rows {
			if len(r) != 4 || r[1] != int64(0) {
				t.Fatalf("%s: %v", tt.sql, rows)
			}
			got = append(got, r[3].(string))
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Fatalf("%s: %q want %q", tt.sql, got, tt.want)
		}
	}
	execTest(t, db, "INSERT INTO t VALUES (1, 1, 'x')")
	queryTest(t, db, "EXPLAIN QUERY PLAN DELETE FROM t")
	queryTest(t, db, "EXPLAIN QUERY PLAN UPDATE t SET a = 2")
	if got := queryTest(t, db, "SELECT a FROM t"); !reflect.DeepEqual(got, [][]Value{{int64(1)}}) {
		t.Fatalf("EXPLAIN modified data: %v", got)
	}
	for _, bad := range []string{"EXPLAIN QUERY PLAN SELECT * FROM missing", "EXPLAIN QUERY PLAN DELETE FROM missing", "EXPLAIN QUERY PLAN UPDATE t SET nope = 1", "EXPLAIN QUERY PLAN INSERT INTO t VALUES (1,1,'x')"} {
		if _, err := db.Query(bg, bad); err == nil {
			t.Fatalf("%s: expected error", bad)
		}
	}
}
