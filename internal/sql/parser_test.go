package sql

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

func mustParse(t *testing.T, input string) Statement {
	t.Helper()
	s, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse(%q): %v", input, err)
	}
	return s
}

func TestStatements(t *testing.T) {
	tests := []struct {
		input string
		want  Statement
	}{
		{"DROP TABLE IF EXISTS t;", &DropTable{Name: "t", IfExists: true}},
		{"DROP INDEX i", &DropIndex{Name: "i"}},
		{"DROP UNIQUE INDEX IF EXISTS i", &DropIndex{Name: "i", Unique: true, IfExists: true}},
		{"CREATE UNIQUE INDEX IF NOT EXISTS i ON t (a,b)", &CreateIndex{Name: "i", Table: "t", Columns: []string{"a", "b"}, Unique: true, IfNotExists: true}},
		{"INSERT INTO t (a,b) VALUES (1,'one'), (2,NULL)", &Insert{Table: "t", Columns: []string{"a", "b"}, Rows: [][]Expr{{&Literal{Value: int64(1)}, &Literal{Value: "one"}}, {&Literal{Value: int64(2)}, &Literal{Value: nil}}}}},
		{"UPDATE t SET a=a+1,b=? WHERE id=?", &Update{Table: "t", Assignments: []Assignment{{Column: "a", Value: &Binary{Op: "+", Left: &Column{Name: "a"}, Right: &Literal{Value: int64(1)}}}, {Column: "b", Value: &Parameter{Index: 0}}}, Where: &Binary{Op: "=", Left: &Column{Name: "id"}, Right: &Parameter{Index: 1}}}},
		{"DELETE FROM t WHERE a IS NULL", &Delete{Table: "t", Where: &Unary{Op: "IS NULL", X: &Column{Name: "a"}}}},
		{"explain query plan SELECT * FROM t", &Explain{Statement: &Select{Columns: []SelectItem{{Expr: &Star{}}}, From: TableRef{Name: "t"}}, QueryPlan: true}},
		{"EXPLAIN SELECT * FROM t", &Explain{Statement: &Select{Columns: []SelectItem{{Expr: &Star{}}}, From: TableRef{Name: "t"}}}},
		{"ALTER TABLE t ADD COLUMN x TEXT NOT NULL DEFAULT 'a'", &AlterTable{Table: "t", Action: "ADD COLUMN", Column: ColumnDef{Name: "x", Type: "TEXT", NotNull: true, Default: &Literal{Value: "a"}}}},
		{"alter table t add x INTEGER REFERENCES p(id)", &AlterTable{Table: "t", Action: "ADD COLUMN", Column: ColumnDef{Name: "x", Type: "INTEGER", References: &ForeignKey{Table: "p", Column: "id"}}}},
		{"CREATE TABLE t (a TEXT, b TEXT, UNIQUE (a, b), UNIQUE (b))", &CreateTable{Name: "t", Columns: []ColumnDef{{Name: "a", Type: "TEXT"}, {Name: "b", Type: "TEXT"}}, Constraints: []TableConstraint{{Kind: "UNIQUE", Column: "a", Columns: []string{"a", "b"}}, {Kind: "UNIQUE", Column: "b"}}}},
		{"ALTER TABLE t DROP COLUMN x", &AlterTable{Table: "t", Action: "DROP COLUMN", From: "x"}},
		{"ALTER TABLE t DROP x", &AlterTable{Table: "t", Action: "DROP COLUMN", From: "x"}},
		{"ALTER TABLE t RENAME TO u", &AlterTable{Table: "t", Action: "RENAME TO", To: "u"}},
		{"ALTER TABLE t RENAME COLUMN a TO b", &AlterTable{Table: "t", Action: "RENAME COLUMN", From: "a", To: "b"}},
		{"ALTER TABLE t RENAME a TO b", &AlterTable{Table: "t", Action: "RENAME COLUMN", From: "a", To: "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := mustParse(t, tt.input)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestCreateTable(t *testing.T) {
	s := mustParse(t, `CREATE TABLE IF NOT EXISTS people (
 id INTEGER PRIMARY KEY,
 name TEXT UNIQUE NOT NULL DEFAULT 'it''s',
 rating REAL DEFAULT -1.25,
 data BLOB DEFAULT X'00ff',
 parent INTEGER REFERENCES people(id) ON DELETE RESTRICT ON UPDATE RESTRICT,
 empty TEXT DEFAULT NULL,
 UNIQUE(name), FOREIGN KEY(parent) REFERENCES people(id) ON UPDATE RESTRICT
 )`).(*CreateTable)
	want := &CreateTable{Name: "people", IfNotExists: true, Columns: []ColumnDef{
		{Name: "id", Type: "INTEGER", PrimaryKey: true},
		{Name: "name", Type: "TEXT", Unique: true, NotNull: true, Default: &Literal{Value: "it's"}},
		{Name: "rating", Type: "REAL", Default: &Literal{Value: float64(-1.25)}},
		{Name: "data", Type: "BLOB", Default: &Literal{Value: []byte{0, 255}}},
		{Name: "parent", Type: "INTEGER", References: &ForeignKey{Table: "people", Column: "id", OnDelete: "RESTRICT", OnUpdate: "RESTRICT"}},
		{Name: "empty", Type: "TEXT", Default: &Literal{Value: nil}},
	}, Constraints: []TableConstraint{{Kind: "UNIQUE", Column: "name"}, {Kind: "FOREIGN KEY", Column: "parent", References: &ForeignKey{Table: "people", Column: "id", OnUpdate: "RESTRICT"}}}}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("got %#v, want %#v", s, want)
	}
	tablePK := mustParse(t, "CREATE TABLE t (id INTEGER, PRIMARY KEY(id))").(*CreateTable)
	if tablePK.Constraints[0].Kind != "PRIMARY KEY" || tablePK.Constraints[0].Column != "id" {
		t.Fatal(tablePK)
	}
}

func TestSelect(t *testing.T) {
	s := mustParse(t, `SELECT DISTINCT p.name AS who, COUNT(*) n, SUM(q.amount), AVG(q.amount), MIN(q.amount), MAX(q.amount)
 FROM people AS p INNER JOIN payments q ON p.id=q.person
 LEFT OUTER JOIN flags f ON f.id=p.id AND f.value IS NOT NULL
 WHERE NOT p.id=0 OR q.amount>?
 GROUP BY p.name HAVING COUNT(*)>?
 ORDER BY who DESC, p.name ASC LIMIT ? OFFSET ?;`).(*Select)
	if !s.Distinct || len(s.Columns) != 6 || s.Columns[0].Alias != "who" || s.Columns[1].Alias != "n" {
		t.Fatalf("columns: %#v", s.Columns)
	}
	if !reflect.DeepEqual(s.From, TableRef{Name: "people", Alias: "p"}) || len(s.Joins) != 2 || s.Joins[0].Type != "INNER" || s.Joins[1].Type != "LEFT" {
		t.Fatalf("joins: %#v", s)
	}
	for i, name := range []string{"COUNT", "SUM", "AVG", "MIN", "MAX"} {
		c := s.Columns[i+1].Expr.(*Call)
		if c.Name != name || c.Star != (i == 0) {
			t.Fatal(c)
		}
	}
	if len(s.GroupBy) != 1 || s.Having == nil || len(s.OrderBy) != 2 || !s.OrderBy[0].Desc || s.OrderBy[1].Desc {
		t.Fatalf("clauses: %#v", s)
	}
	if ParameterCount(s) != 4 || s.Limit.(*Parameter).Index != 2 || s.Offset.(*Parameter).Index != 3 {
		t.Fatalf("parameters: %#v", s)
	}
	stars := mustParse(t, `SELECT *, p.* FROM people p JOIN flags f ON p.id=f.id LIMIT 10 OFFSET 0`).(*Select)
	if stars.Columns[1].Expr.(*Star).Table != "p" || stars.Joins[0].Type != "INNER" || stars.Limit.(*Literal).Value != int64(10) {
		t.Fatal(stars)
	}
}

func TestExpressionPrecedence(t *testing.T) {
	col := func(s string) Expr { return &Column{Name: s} }
	lit := func(n int64) Expr { return &Literal{Value: n} }
	bin := func(op string, l, r Expr) Expr { return &Binary{Op: op, Left: l, Right: r} }
	tests := []struct {
		input string
		want  Expr
	}{
		{"1+2*3-4/2", bin("-", bin("+", lit(1), bin("*", lit(2), lit(3))), bin("/", lit(4), lit(2)))},
		{"8/4/2", bin("/", bin("/", lit(8), lit(4)), lit(2))},
		{"(1+2)*3", bin("*", bin("+", lit(1), lit(2)), lit(3))},
		{"NOT a=1 AND b=2 OR c=3", bin("OR", bin("AND", &Unary{Op: "NOT", X: bin("=", col("a"), lit(1))}, bin("=", col("b"), lit(2))), bin("=", col("c"), lit(3)))},
		{"a IS NOT NULL", &Unary{Op: "IS NOT NULL", X: col("a")}},
		{"-a*+b", bin("*", &Unary{Op: "-", X: col("a")}, &Unary{Op: "+", X: col("b")})},
		{"NOT a IS NULL", &Unary{Op: "NOT", X: &Unary{Op: "IS NULL", X: col("a")}}},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := mustParse(t, "SELECT "+tt.input).(*Select).Columns[0].Expr
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %#v, want %#v", got, tt.want)
			}
		})
	}
	for _, op := range []string{"=", "!=", "<>", "<", ">", "<=", ">="} {
		got := mustParse(t, "SELECT a"+op+"b").(*Select).Columns[0].Expr.(*Binary)
		if got.Op != op {
			t.Fatal(got)
		}
	}
}

func TestFunctionNamesAsIdentifiers(t *testing.T) {
	for _, name := range []string{"count", "sum", "avg", "min", "max", "AvG"} {
		t.Run(name, func(t *testing.T) {
			table := mustParse(t, "CREATE TABLE "+name+" ("+name+" INTEGER, UNIQUE("+name+"))").(*CreateTable)
			if table.Name != name || table.Columns[0].Name != name || table.Constraints[0].Column != name {
				t.Fatalf("identifier spelling lost: %#v", table)
			}
			s := mustParse(t, "SELECT "+name+", t."+name+" AS "+name+", AVG("+name+") "+name+" FROM "+name+" AS t ORDER BY "+name).(*Select)
			if s.Columns[0].Expr.(*Column).Name != name || s.Columns[1].Expr.(*Column).Name != name || s.Columns[1].Alias != name || s.Columns[2].Alias != name || s.Columns[2].Expr.(*Call).Name != "AVG" || s.OrderBy[0].Expr.(*Column).Name != name {
				t.Fatalf("incorrect name context: %#v", s)
			}
			for _, input := range []string{
				"SELECT " + name + ".* FROM t " + name,
				"CREATE INDEX " + name + " ON t (" + name + ")",
				"INSERT INTO t(" + name + ") VALUES(1)",
				"UPDATE t SET " + name + "=" + name + "+1",
				"DELETE FROM t WHERE " + name + "=1",
			} {
				mustParse(t, input)
			}
		})
	}
}

func TestGenericFunctionCalls(t *testing.T) {
	s := mustParse(t, `SELECT lower(name), UPPER(name), length(name), abs(-amount), coalesce(NULL, ?, lower(?)), mystery(), mystery(1, 2), "custom"(3), count(*), sum(*) FROM t`).(*Select)
	wantNames := []string{"LOWER", "UPPER", "LENGTH", "ABS", "COALESCE", "MYSTERY", "MYSTERY", "CUSTOM", "COUNT", "SUM"}
	wantArity := []int{1, 1, 1, 1, 3, 0, 2, 1, 0, 0}
	for i, name := range wantNames {
		call, ok := s.Columns[i].Expr.(*Call)
		if !ok || call.Name != name || len(call.Args) != wantArity[i] || call.Star != (i >= 8) {
			t.Fatalf("call %d: %#v", i, s.Columns[i].Expr)
		}
	}
	coalesce := s.Columns[4].Expr.(*Call)
	if coalesce.Args[1].(*Parameter).Index != 0 || coalesce.Args[2].(*Call).Args[0].(*Parameter).Index != 1 || ParameterCount(s) != 2 {
		t.Fatal("nested call parameters lost")
	}
	// Arity and function availability belong to binding, not SQL syntax parsing.
	for _, input := range []string{"SELECT COUNT()", "SELECT MAX(a,b)", "SELECT unknown_func(a)", "SELECT COALESCE()", "SELECT unknown_func(*)"} {
		mustParse(t, input)
	}
}

func TestLiteralValues(t *testing.T) {
	s := mustParse(t, `SELECT 9223372036854775807,-9223372036854775808,1.25,1e2,.5,1.,'a''b',X'0041ff',NULL`).(*Select)
	values := []any{int64(math.MaxInt64), int64(math.MinInt64), float64(1.25), float64(100), float64(.5), float64(1), "a'b", []byte{0, 65, 255}, nil}
	for i, want := range values {
		got := s.Columns[i].Expr.(*Literal).Value
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("literal %d: %#v != %#v", i, got, want)
		}
	}
	s = mustParse(t, `select "odd""name"."select" from "table" -- ignored ?
 where "select"=? /* ? */`).(*Select)
	if s.Columns[0].Expr.(*Column).Table != `odd"name` || s.From.Name != "table" || ParameterCount(s) != 1 {
		t.Fatal(s)
	}
}

func TestRejectUnsupportedAndMalformed(t *testing.T) {
	tests := []string{
		"", ";", "SELECT", "SELECT 1; SELECT 2", "SELECT 1;;", "SELECT 1 garbage extra", "SELECT * FROM",
		"SELECT * FROM t RIGHT JOIN u ON t.a=u.a", "SELECT * FROM t CROSS JOIN u", "SELECT * FROM t,u",
		"SELECT * FROM t LEFT JOIN u", "SELECT * FROM t JOIN u USING(a)", "SELECT (SELECT a FROM t)",
		"CREATE TABLE t (a TEXT, UNIQUE (a, a))", "CREATE TABLE t (a TEXT, b TEXT, PRIMARY KEY (a, b))", "CREATE TABLE t (a TEXT, b TEXT, FOREIGN KEY (a, b) REFERENCES u(a))",
		"ALTER TABLE t", "ALTER t ADD x TEXT", "ALTER TABLE t ADD x", "ALTER TABLE t RENAME a b", "ALTER TABLE t MODIFY x TEXT",
		"SELECT 1 UNION SELECT 2", "SELECT a IS 1",
		"SELECT a IS NOT", "SELECT *+1", "SELECT COUNT(DISTINCT a)", "SELECT abs(,a)", "SELECT abs(a,)", "SELECT f(*,a)", "SELECT f(a,*)", "SELECT f(",
		"SELECT t.* AS x", "SELECT TRUE", "SELECT 1e", "SELECT 1e9999", "SELECT 9223372036854775808", "SELECT 9223372036854775809",
		"SELECT * FROM t LIMIT -1", "SELECT * FROM t LIMIT 1.5", "SELECT * FROM t LIMIT a", "SELECT * FROM t LIMIT 1,2", "SELECT * FROM t OFFSET -1",
		"SELECT 'unclosed", "SELECT X'0'", "SELECT X'gg'", "SELECT \"\"", "SELECT 1 /* unclosed", "SELECT \x00", "SELECT \xff",
		"CREATE TABLE t ()", "CREATE TABLE t (x BOOLEAN)", "CREATE TABLE t (x INTEGER,)", "CREATE TABLE t (x INTEGER CHECK(x>0))",
		"CREATE TABLE t (x INTEGER DEFAULT ?)", "CREATE TABLE t (x INTEGER DEFAULT (1))", "CREATE TABLE t (x TEXT DEFAULT -'x')", "CREATE TABLE t (x INTEGER DEFAULT +NULL)",
		"CREATE TABLE t (x INTEGER DEFAULT 9223372036854775808)", "CREATE TABLE t (x INTEGER DEFAULT 1 DEFAULT 2)",
		"CREATE TABLE t (x INTEGER PRIMARY KEY,y INTEGER PRIMARY KEY)", "CREATE TABLE t (x INTEGER,X TEXT)",
		"CREATE TABLE t (x INTEGER,PRIMARY KEY(x,y))", "CREATE TABLE t (x INTEGER,UNIQUE(x,y))", "CREATE TABLE t (x INTEGER,PRIMARY KEY(y))",
		"CREATE TABLE t (x INTEGER REFERENCES u(a,b))", "CREATE TABLE t (x INTEGER REFERENCES u(a) ON DELETE CASCADE)",
		"CREATE TABLE t (x INTEGER REFERENCES u(a) ON DELETE RESTRICT ON DELETE RESTRICT)", "CREATE TABLE t (x INTEGER,CONSTRAINT c UNIQUE(x))",
		"CREATE INDEX i ON t ()", "CREATE INDEX i ON t (a DESC)", "DROP UNIQUE TABLE t",
		"INSERT t VALUES(1)", "INSERT INTO t VALUES()", "INSERT INTO t VALUES(1),(1,2)", "INSERT INTO t(a,b) VALUES(1)",
		"INSERT INTO t SELECT 1", "INSERT INTO t DEFAULT VALUES", "UPDATE t SET", "UPDATE t SET a=", "DELETE t", "EXPLAIN EXPLAIN SELECT 1", "EXPLAIN QUERY SELECT 1", "EXPLAIN QUERY PLAN EXPLAIN SELECT 1",
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			s, err := Parse(input)
			if err == nil || s != nil {
				t.Fatalf("accepted %q: %#v (%v)", input, s, err)
			}
			var positioned *Error
			if !errors.As(err, &positioned) || positioned.Pos < 0 || positioned.Pos > len(input) {
				t.Fatalf("invalid error: %v", err)
			}
		})
	}
}

func TestDepthLimit(t *testing.T) {
	for _, input := range []string{"SELECT " + strings.Repeat("(", 300) + "1" + strings.Repeat(")", 300), "SELECT " + strings.Repeat("NOT ", 300) + "1", "SELECT " + strings.Repeat("SUM(", 300) + "1" + strings.Repeat(")", 300)} {
		if _, err := Parse(input); err == nil {
			t.Fatal("accepted excessive nesting")
		}
	}
	if _, err := Parse("SELECT " + strings.Repeat("(", 100) + "1" + strings.Repeat(")", 100)); err != nil {
		t.Fatal(err)
	}
}

func TestWalkParameters(t *testing.T) {
	tests := []struct {
		input string
		count int
	}{
		{"EXPLAIN SELECT ?+SUM(?) FROM t JOIN u ON t.id=? WHERE a=? GROUP BY ? HAVING COUNT(*)>? ORDER BY ? LIMIT ? OFFSET ?", 9},
		{"INSERT INTO t VALUES(?+?,SUM(?)),(?,?)", 5},
		{"UPDATE t SET a=-?,b=? WHERE NOT a=?", 3},
		{"DELETE FROM t WHERE a IS NOT NULL AND b=?", 1},
		{"CREATE TABLE t (a INTEGER DEFAULT NULL)", 0},
		{"DROP TABLE t", 0},
	}
	for _, tt := range tests {
		s := mustParse(t, tt.input)
		if n := ParameterCount(s); n != tt.count {
			t.Fatalf("%q: got %d want %d", tt.input, n, tt.count)
		}
	}
	s := mustParse(t, "SELECT a+1*b")
	visited := 0
	WalkStatement(s, func(e Expr) bool { visited++; _, binary := e.(*Binary); return !binary })
	if visited != 1 {
		t.Fatal(visited)
	}
	if ParameterCount(nil) != 0 {
		t.Fatal("nil statement")
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{"SELECT 1", "SELECT ?+? FROM t WHERE a IS NULL", "CREATE TABLE t (a INTEGER PRIMARY KEY,b BLOB DEFAULT X'ff')", "INSERT INTO t VALUES(1),(2)", "SELECT COUNT(*) FROM t GROUP BY a HAVING SUM(a)>1", "SELECT coalesce(?,lower(?)) AS avg FROM t", "SELECT unknown_func(), avg FROM t", "SELECT " + strings.Repeat("(", 260) + "1", "\xff", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, input string) {
		s, err := Parse(input)
		if err != nil {
			if s != nil {
				t.Fatal("statement returned with error")
			}
			var e *Error
			if !errors.As(err, &e) || e.Pos < 0 || e.Pos > len(input) {
				t.Fatalf("invalid error %v", err)
			}
			return
		}
		if s == nil {
			t.Fatal("nil successful statement")
		}
		tokens, err := Lex(input)
		if err != nil {
			t.Fatal(err)
		}
		expected := 0
		for _, tok := range tokens {
			if tok.Kind == TokenParameter {
				expected++
			}
		}
		if ParameterCount(s) != expected {
			t.Fatalf("parameter count %d != %d", ParameterCount(s), expected)
		}
	})
}
