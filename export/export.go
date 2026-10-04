// Package export writes a gbase database as DDL, CSV data, and a load script
// for PostgreSQL or Oracle, and reports where the target would treat the
// data differently.
//
// The output directory holds schema.sql (tables), one CSV file per table,
// constraints.sql (indexes and foreign keys, applied after the data), and
// REPORT.md. PostgreSQL loads with psql: cd DIR && psql -d DB -f load.sql.
// Oracle loads with SQL*Loader: DIR/load.sh user/password@service, which also
// uses one .ctl file per table.
//
// Names are written in the target's folded case (lowercase for PostgreSQL,
// uppercase for Oracle) so unquoted SQL keeps working; names that are reserved
// or not plain identifiers are quoted and reported. INTEGER PRIMARY KEY
// becomes an identity column starting after every ID gbase has issued.
package export

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rey4L/gbase"
)

type Dialect string

const (
	Postgres Dialect = "postgres"
	Oracle   Dialect = "oracle"
)

// Report summarizes an export. Warnings name data or schema the target
// rejects or changes; review them before loading.
type Report struct {
	Dialect  Dialect
	Tables   []TableReport
	Warnings []string
}

type TableReport struct {
	Name, File string
	Rows       int64
}

// Oracle VARCHAR2 holds at most this many bytes without MAX_STRING_SIZE=EXTENDED.
const oracleVarcharBytes = 4000

// Export writes db to dir, which must not exist or be empty. It reads the
// schema and data in one transaction, so writers wait until it finishes.
func Export(ctx context.Context, db *gbase.DB, dir string, d Dialect) (*Report, error) {
	if d != Postgres && d != Oracle {
		return nil, fmt.Errorf("export: unknown dialect %q; use postgres or oracle", d)
	}
	if e := os.MkdirAll(dir, 0o755); e != nil {
		return nil, e
	}
	if entries, e := os.ReadDir(dir); e != nil {
		return nil, e
	} else if len(entries) > 0 {
		return nil, fmt.Errorf("export: %s is not empty", dir)
	}
	tx, e := db.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	tables, e := tx.Describe()
	if e != nil {
		return nil, e
	}
	x := &exporter{ctx: ctx, tx: tx, dir: dir, d: d, report: &Report{Dialect: d}, warned: map[string]bool{}}
	for i := range tables {
		if e = x.table(i, &tables[i]); e != nil {
			return nil, e
		}
	}
	if e = x.writeScripts(tables); e != nil {
		return nil, e
	}
	return x.report, x.writeReport()
}

type exporter struct {
	ctx    context.Context
	tx     *gbase.Tx
	dir    string
	d      Dialect
	report *Report
	warned map[string]bool
	stats  []tableStats
}

type tableStats struct {
	file    string
	columns []columnStats
}

type columnStats struct {
	maxBytes int // longest TEXT value or BLOB, in bytes
}

func (x *exporter) warn(format string, args ...any) {
	w := fmt.Sprintf(format, args...)
	if !x.warned[w] {
		x.warned[w] = true
		x.report.Warnings = append(x.report.Warnings, w)
	}
}

var plainName = regexp.MustCompile(`[^A-Za-z0-9_]+`)

// table writes one table's CSV file and gathers what the DDL needs.
func (x *exporter) table(n int, t *gbase.TableInfo) error {
	base := fmt.Sprintf("%02d_%s", n+1, strings.Trim(plainName.ReplaceAllString(strings.ToLower(t.Name), "_"), "_"))
	st := tableStats{file: base, columns: make([]columnStats, len(t.Columns))}
	f, e := os.Create(filepath.Join(x.dir, base+".csv"))
	if e != nil {
		return e
	}
	defer f.Close()
	terminator := "\n"
	if x.d == Oracle {
		// SQL*Loader splits records at newlines even inside quotes, so records
		// end with a record separator the control file names instead.
		terminator = "\x1e\n"
	}
	var header []string
	for _, c := range t.Columns {
		header = append(header, csvQuote(c.Name))
	}
	if _, e = f.WriteString(strings.Join(header, ",") + terminator); e != nil {
		return e
	}
	query := "SELECT * FROM " + gbaseIdent(t.Name)
	for _, c := range t.Columns {
		if c.PrimaryKey && c.StorageType == "INTEGER" {
			query += " ORDER BY " + gbaseIdent(c.Name)
		}
	}
	rows, e := x.tx.Query(x.ctx, query)
	if e != nil {
		return e
	}
	defer rows.Close()
	partial := x.partialNullTrackers(t)
	empty := make([]int, len(t.Columns))
	var count int64
	fields := make([]string, len(t.Columns))
	for rows.Next() {
		values := rows.Values()
		count++
		for i, c := range t.Columns {
			v := values[i]
			switch s := v.(type) {
			case string:
				st.columns[i].maxBytes = max(st.columns[i].maxBytes, len(s))
				if s == "" {
					empty[i]++
				}
			case []byte:
				st.columns[i].maxBytes = max(st.columns[i].maxBytes, len(s))
			}
			if fields[i], e = x.field(base, count, i, c, v); e != nil {
				return e
			}
		}
		for _, p := range partial {
			p.add(values)
		}
		if _, e = f.WriteString(strings.Join(fields, ",") + terminator); e != nil {
			return e
		}
	}
	if e = rows.Err(); e != nil {
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if x.d == Oracle {
		for i, c := range t.Columns {
			if empty[i] == 0 {
				continue
			}
			if c.NotNull {
				x.warn("%s.%s: %d empty strings will fail to load: Oracle stores '' as NULL and the column is NOT NULL", t.Name, c.Name, empty[i])
			} else {
				x.warn("%s.%s: %d empty strings will load as NULL, since Oracle stores '' as NULL", t.Name, c.Name, empty[i])
			}
		}
		for _, p := range partial {
			if p.duplicates > 0 {
				x.warn("%s UNIQUE (%s): %d rows repeat the non-NULL values of another row with NULLs in the rest; Oracle rejects these, while gbase and PostgreSQL allow them", t.Name, strings.Join(p.columns, ", "), p.duplicates)
			}
		}
	}
	x.stats = append(x.stats, st)
	x.report.Tables = append(x.report.Tables, TableReport{Name: t.Name, File: base + ".csv", Rows: count})
	return nil
}

// field renders one CSV field. NULL is an empty unquoted field and text is
// always quoted, so PostgreSQL keeps ” distinct from NULL.
func (x *exporter) field(base string, row int64, col int, c gbase.ColumnInfo, v gbase.Value) (string, error) {
	switch s := v.(type) {
	case nil:
		return "", nil
	case int64:
		if c.Type == "BOOLEAN" && x.d == Postgres {
			if s != 0 {
				return "t", nil
			}
			return "f", nil
		}
		return strconv.FormatInt(s, 10), nil
	case float64:
		return strconv.FormatFloat(s, 'g', -1, 64), nil
	case string:
		if x.d == Oracle && (c.Type == "DATETIME" || c.Type == "TIMESTAMP") {
			// SQL*Loader masks cannot match the literal T and Z, so use a space and drop the zone.
			if t, e := time.Parse(time.RFC3339Nano, s); e == nil {
				s = t.UTC().Format("2006-01-02 15:04:05.000000")
			}
		}
		return csvQuote(s), nil
	case []byte:
		if x.d == Postgres {
			return `\x` + hex.EncodeToString(s), nil
		}
		// Oracle loads each BLOB from its own file, which has no size limit.
		name := filepath.Join("blobs", base, fmt.Sprintf("%d_%d.bin", row, col+1))
		if e := os.MkdirAll(filepath.Join(x.dir, filepath.Dir(name)), 0o755); e != nil {
			return "", e
		}
		if e := os.WriteFile(filepath.Join(x.dir, name), s, 0o644); e != nil {
			return "", e
		}
		return csvQuote(filepath.ToSlash(name)), nil
	}
	return "", fmt.Errorf("export: unexpected value %T", v)
}

func csvQuote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func gbaseIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// partialNull finds rows of a multi-column UNIQUE that Oracle treats as
// duplicates: Oracle compares the non-NULL columns when some, but not all,
// are NULL, and it stores ” as NULL.
type partialNull struct {
	columns    []string
	positions  []int
	seen       map[string]bool
	duplicates int
}

func (x *exporter) partialNullTrackers(t *gbase.TableInfo) []*partialNull {
	if x.d != Oracle {
		return nil
	}
	var out []*partialNull
	for _, i := range t.Indexes {
		if !i.Unique || len(i.Columns) < 2 {
			continue
		}
		p := &partialNull{columns: i.Columns, seen: map[string]bool{}}
		for _, name := range i.Columns {
			for k, c := range t.Columns {
				if strings.EqualFold(c.Name, name) {
					p.positions = append(p.positions, k)
				}
			}
		}
		out = append(out, p)
	}
	return out
}

func (p *partialNull) add(values []gbase.Value) {
	var key strings.Builder
	nulls := 0
	for _, k := range p.positions {
		v := values[k]
		if s, ok := v.(string); ok && s == "" {
			v = nil
		}
		if v == nil {
			nulls++
			key.WriteString("N|")
			continue
		}
		fmt.Fprintf(&key, "%T:%q|", v, fmt.Sprint(v))
	}
	if nulls == 0 || nulls == len(p.positions) {
		return
	}
	if p.seen[key.String()] {
		p.duplicates++
	}
	p.seen[key.String()] = true
}

// ident renders a gbase name in the target's folded case, quoting it when it
// is reserved or not a plain identifier.
func (x *exporter) ident(name string) string {
	if x.d == Postgres {
		s := strings.ToLower(name)
		if len(s) > 63 {
			x.warn("%s: PostgreSQL truncates names to 63 bytes", name)
		}
		if postgresPlain.MatchString(s) && !postgresReserved[strings.ToUpper(s)] {
			return s
		}
		if postgresReserved[strings.ToUpper(s)] {
			x.warn("%s is reserved in PostgreSQL; it is exported quoted, so SQL must write it as \"%s\"", name, s)
		} else {
			x.warn("%s is not a plain PostgreSQL identifier; it is exported quoted", name)
		}
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	s := strings.ToUpper(name)
	if len(s) > 128 {
		x.warn("%s: Oracle limits names to 128 bytes", name)
	}
	if oraclePlain.MatchString(s) && !oracleReserved[s] {
		return s
	}
	if oracleReserved[s] {
		x.warn("%s is reserved in Oracle; it is exported quoted, so SQL must write it as \"%s\"", name, s)
	} else {
		x.warn("%s is not a plain Oracle identifier; it is exported quoted", name)
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

var (
	postgresPlain = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	oraclePlain   = regexp.MustCompile(`^[A-Z][A-Z0-9_$#]*$`)
)

func (x *exporter) columnType(c gbase.ColumnInfo, st columnStats) string {
	if x.d == Postgres {
		switch c.Type {
		case "INTEGER":
			return "BIGINT"
		case "REAL":
			return "DOUBLE PRECISION"
		case "BLOB":
			return "BYTEA"
		case "DATETIME", "TIMESTAMP":
			return "TIMESTAMPTZ"
		}
		return c.Type // TEXT, BOOLEAN, DATE
	}
	switch c.Type {
	case "INTEGER":
		return "NUMBER(19)"
	case "REAL":
		return "BINARY_DOUBLE"
	case "TEXT":
		// gbase caps index keys at 1024 bytes, so a column long enough to need
		// CLOB is never in a key or index, which CLOB would not allow.
		if st.maxBytes > oracleVarcharBytes {
			return "CLOB"
		}
		return fmt.Sprintf("VARCHAR2(%d BYTE)", oracleVarcharBytes)
	case "BLOB":
		return "BLOB"
	case "BOOLEAN":
		return "NUMBER(1)"
	case "DATE":
		return "DATE"
	}
	return "TIMESTAMP(6)" // DATETIME, TIMESTAMP: UTC values
}

func (x *exporter) literal(c gbase.ColumnInfo, v gbase.Value) string {
	switch s := v.(type) {
	case nil:
		return "NULL"
	case int64:
		if c.Type == "BOOLEAN" && x.d == Postgres {
			return strings.ToUpper(strconv.FormatBool(s != 0))
		}
		return strconv.FormatInt(s, 10)
	case float64:
		if math.IsInf(s, 0) || math.IsNaN(s) {
			return "NULL"
		}
		return strconv.FormatFloat(s, 'g', -1, 64)
	case string:
		if x.d == Oracle {
			switch c.Type {
			case "DATE":
				return "DATE '" + s + "'"
			case "DATETIME", "TIMESTAMP":
				if t, e := time.Parse(time.RFC3339Nano, s); e == nil {
					return "TIMESTAMP '" + t.UTC().Format("2006-01-02 15:04:05.000000") + "'"
				}
			}
		}
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	case []byte:
		if x.d == Postgres {
			return `'\x` + hex.EncodeToString(s) + `'::bytea`
		}
		return "HEXTORAW('" + hex.EncodeToString(s) + "')"
	}
	return "NULL"
}

func (x *exporter) writeScripts(tables []gbase.TableInfo) error {
	var schema, constraints strings.Builder
	if x.d == Oracle {
		schema.WriteString("WHENEVER SQLERROR EXIT FAILURE\n\n")
		constraints.WriteString("WHENEVER SQLERROR EXIT FAILURE\n\n")
	}
	for n := range tables {
		t := &tables[n]
		st := x.stats[n]
		var defs []string
		for i, c := range t.Columns {
			name := x.ident(c.Name)
			d := name + " " + x.columnType(c, st.columns[i])
			if c.PrimaryKey && c.StorageType == "INTEGER" {
				d += fmt.Sprintf(" GENERATED BY DEFAULT AS IDENTITY (START WITH %d)", t.NextID)
			}
			if c.HasDefault && c.Default != nil {
				if x.d == Oracle {
					if s, ok := c.Default.(string); ok && s == "" {
						x.warn("%s.%s: DEFAULT '' is NULL in Oracle", t.Name, c.Name)
					}
				}
				d += " DEFAULT " + x.literal(c, c.Default)
			}
			if c.NotNull && !c.PrimaryKey {
				d += " NOT NULL"
			}
			if c.PrimaryKey {
				d += " PRIMARY KEY"
			} else if c.Unique {
				d += " UNIQUE"
			}
			if c.Type == "BOOLEAN" && x.d == Oracle {
				d += " CHECK (" + name + " IN (0, 1))"
			}
			defs = append(defs, d)
		}
		for _, i := range t.Indexes {
			if i.Constraint {
				defs = append(defs, "UNIQUE ("+x.idents(i.Columns)+")")
			}
		}
		fmt.Fprintf(&schema, "CREATE TABLE %s (\n  %s\n);\n\n", x.ident(t.Name), strings.Join(defs, ",\n  "))
		for _, i := range t.Indexes {
			if i.Constraint {
				continue
			}
			unique := ""
			if i.Unique {
				unique = "UNIQUE "
			}
			fmt.Fprintf(&constraints, "CREATE %sINDEX %s ON %s (%s);\n", unique, x.ident(i.Name), x.ident(t.Name), x.idents(i.Columns))
		}
		for _, c := range t.Columns {
			if c.References != nil {
				// RESTRICT is PostgreSQL's NO ACTION checked at once, and Oracle's only behavior.
				action := ""
				if x.d == Postgres {
					action = " ON DELETE RESTRICT ON UPDATE RESTRICT"
				}
				fmt.Fprintf(&constraints, "ALTER TABLE %s ADD FOREIGN KEY (%s) REFERENCES %s (%s)%s;\n", x.ident(t.Name), x.ident(c.Name), x.ident(c.References.Table), x.ident(c.References.Column), action)
			}
		}
	}
	if x.d == Oracle {
		schema.WriteString("EXIT\n")
		constraints.WriteString("EXIT\n")
	}
	if e := x.write("schema.sql", schema.String()); e != nil {
		return e
	}
	if e := x.write("constraints.sql", constraints.String()); e != nil {
		return e
	}
	if x.d == Postgres {
		return x.postgresLoad(tables)
	}
	return x.oracleLoad(tables)
}

func (x *exporter) idents(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = x.ident(n)
	}
	return strings.Join(out, ", ")
}

func (x *exporter) write(name, content string) error {
	return os.WriteFile(filepath.Join(x.dir, name), []byte(content), 0o644)
}

func (x *exporter) postgresLoad(tables []gbase.TableInfo) error {
	var b strings.Builder
	b.WriteString("-- Run from this directory: psql -d DATABASE -f load.sql\n\\set ON_ERROR_STOP on\nBEGIN;\n\\i schema.sql\n")
	for n, t := range tables {
		fmt.Fprintf(&b, "\\copy %s FROM '%s.csv' WITH (FORMAT csv, HEADER true)\n", x.ident(t.Name), x.stats[n].file)
	}
	b.WriteString("\\i constraints.sql\nCOMMIT;\n")
	return x.write("load.sql", b.String())
}

func (x *exporter) oracleLoad(tables []gbase.TableInfo) error {
	var sh strings.Builder
	sh.WriteString("#!/bin/sh\n# Usage: ./load.sh user/password@service\nset -eu\ncd \"$(dirname \"$0\")\"\nconn=\"${1:?usage: ./load.sh user/password@service}\"\nsqlplus -s -L \"$conn\" @schema.sql\n")
	for n := range tables {
		t := &tables[n]
		st := x.stats[n]
		var fields []string
		for i, c := range t.Columns {
			name := x.ident(c.Name)
			switch c.Type {
			case "TEXT":
				fields = append(fields, fmt.Sprintf("%s CHAR(%d)", name, max(st.columns[i].maxBytes, 1)))
			case "DATE":
				fields = append(fields, name+` DATE "YYYY-MM-DD"`)
			case "DATETIME", "TIMESTAMP":
				fields = append(fields, name+` TIMESTAMP "YYYY-MM-DD HH24:MI:SS.FF6"`)
			case "BLOB":
				filler := fmt.Sprintf("LOB_FILE_%d", i+1)
				fields = append(fields, filler+" FILLER CHAR(4000)", fmt.Sprintf("%s LOBFILE(%s) TERMINATED BY EOF NULLIF %s = BLANKS", name, filler, filler))
			default:
				fields = append(fields, name)
			}
		}
		ctl := fmt.Sprintf("OPTIONS (SKIP=1, ERRORS=0)\nLOAD DATA\nCHARACTERSET AL32UTF8\nINFILE '%s.csv' \"str X'1E0A'\"\nAPPEND INTO TABLE %s\nFIELDS TERMINATED BY ',' OPTIONALLY ENCLOSED BY '\"'\nTRAILING NULLCOLS\n(\n  %s\n)\n", st.file, x.ident(t.Name), strings.Join(fields, ",\n  "))
		if e := x.write(st.file+".ctl", ctl); e != nil {
			return e
		}
		fmt.Fprintf(&sh, "sqlldr userid=\"$conn\" control=%[1]s.ctl log=%[1]s.log bad=%[1]s.bad silent=header,feedback\n", st.file)
	}
	sh.WriteString("sqlplus -s -L \"$conn\" @constraints.sql\n")
	if e := x.write("load.sh", sh.String()); e != nil {
		return e
	}
	return os.Chmod(filepath.Join(x.dir, "load.sh"), 0o755)
}

func (x *exporter) writeReport() error {
	var b strings.Builder
	fmt.Fprintf(&b, "# gbase export for %s\n\n", map[Dialect]string{Postgres: "PostgreSQL", Oracle: "Oracle"}[x.d])
	if x.d == Postgres {
		b.WriteString("Load into an empty database from this directory:\n\n    psql -d DATABASE -f load.sql\n\nDATETIME and TIMESTAMP columns become TIMESTAMPTZ.\n\n")
	} else {
		b.WriteString("Load into an empty schema with SQL*Plus and SQL*Loader on the PATH:\n\n    ./load.sh user/password@service\n\nDATETIME and TIMESTAMP columns become TIMESTAMP(6) holding UTC, since Oracle forbids zoned timestamps in keys. BOOLEAN becomes NUMBER(1) checked to 0 or 1.\n\n")
	}
	b.WriteString("| Table | File | Rows |\n|---|---|---|\n")
	for _, t := range x.report.Tables {
		fmt.Fprintf(&b, "| %s | %s | %d |\n", t.Name, t.File, t.Rows)
	}
	sort.Strings(x.report.Warnings)
	if len(x.report.Warnings) == 0 {
		b.WriteString("\nNo warnings.\n")
	} else {
		b.WriteString("\n## Warnings\n\n")
		for _, w := range x.report.Warnings {
			b.WriteString("- " + w + "\n")
		}
	}
	return x.write("REPORT.md", b.String())
}
