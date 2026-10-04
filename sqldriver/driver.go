// Package sqldriver registers gbase with database/sql under the name "gbase".
//
//	import _ "github.com/rey4L/gbase/sqldriver"
//	db, err := sql.Open("gbase", "app.db?busy_timeout=5s")
//
// Connections to the same file share one gbase.DB, so only one transaction
// runs at a time; a statement that waits longer than busy_timeout (default 5s)
// for another transaction fails with gbase.ErrLocked. busy_timeout accepts a
// Go duration or, as in SQLite drivers, milliseconds; _busy_timeout is an alias.
//
// Statements may use ? or named parameters (:name, @name, $name) bound with
// sql.Named, but not both. Exec runs several semicolon-separated statements in
// one transaction. Query results are read fully before Query returns, which
// releases the database at once and allows further statements in the same
// transaction while iterating.
//
// time.Time arguments are stored in the canonical TIMESTAMP form and bool as
// 0 or 1. Columns declared BOOLEAN scan as bool, and DATE, DATETIME, and
// TIMESTAMP as time.Time in UTC. Conn.Raw exposes *Conn, whose DB method
// reaches gbase-specific operations such as Backup.
package sqldriver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rey4L/gbase"
	gsql "github.com/rey4L/gbase/internal/sql"
)

func init() { sql.Register("gbase", Driver{}) }

const defaultBusyTimeout = 5 * time.Second

type Driver struct{}

func (d Driver) Open(dsn string) (driver.Conn, error) {
	c, e := d.OpenConnector(dsn)
	if e != nil {
		return nil, e
	}
	return c.Connect(context.Background())
}

func (Driver) OpenConnector(dsn string) (driver.Connector, error) {
	path, query, _ := strings.Cut(strings.TrimPrefix(dsn, "file:"), "?")
	if path == "" {
		return nil, errors.New("gbase: DSN needs a database path")
	}
	params, e := url.ParseQuery(query)
	if e != nil {
		return nil, fmt.Errorf("gbase: DSN: %w", e)
	}
	c := &connector{path: path, busy: defaultBusyTimeout}
	for k, v := range params {
		switch k {
		case "busy_timeout", "_busy_timeout":
			if c.busy, e = parseTimeout(v[len(v)-1]); e != nil {
				return nil, e
			}
		default:
			return nil, fmt.Errorf("gbase: unknown DSN parameter %q", k)
		}
	}
	return c, nil
}

func parseTimeout(s string) (time.Duration, error) {
	if ms, e := strconv.ParseInt(s, 10, 64); e == nil && ms >= 0 {
		return time.Duration(ms) * time.Millisecond, nil
	}
	if d, e := time.ParseDuration(s); e == nil && d >= 0 {
		return d, nil
	}
	return 0, fmt.Errorf("gbase: invalid busy_timeout %q", s)
}

type connector struct {
	path string
	busy time.Duration
}

func (c *connector) Driver() driver.Driver { return Driver{} }

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	s, e := acquire(c.path, c.busy)
	if e != nil {
		return nil, e
	}
	return &Conn{shared: s}, nil
}

// shared is one open database and the connections using it. gbase locks a
// file for one gbase.DB, so every connection to a path shares that DB.
type shared struct {
	key  string
	db   *gbase.DB
	busy time.Duration
	refs int
}

var registry = struct {
	sync.Mutex
	open map[string]*shared
}{open: map[string]*shared{}}

func acquire(path string, busy time.Duration) (*shared, error) {
	key, e := filepath.Abs(path)
	if e != nil {
		return nil, e
	}
	if resolved, e := filepath.EvalSymlinks(key); e == nil {
		key = resolved
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	registry.Lock()
	defer registry.Unlock()
	if s := registry.open[key]; s != nil {
		if s.busy != busy {
			return nil, fmt.Errorf("gbase: %s is open with busy_timeout %v", path, s.busy)
		}
		s.refs++
		return s, nil
	}
	db, e := gbase.Open(key)
	if e != nil {
		return nil, e
	}
	db.SetBusyTimeout(busy)
	s := &shared{key: key, db: db, busy: busy, refs: 1}
	registry.open[key] = s
	return s, nil
}

func (s *shared) release() error {
	registry.Lock()
	defer registry.Unlock()
	if s.refs--; s.refs > 0 {
		return nil
	}
	delete(registry.open, s.key)
	return s.db.Close()
}

// Conn is a database/sql connection. Reach it with sql.Conn.Raw.
type Conn struct {
	shared   *shared
	tx       *gbase.Tx
	readOnly bool
	closed   bool
}

// DB returns the shared gbase database, for operations such as Backup and
// Check. Calling them while this connection has an open transaction waits for
// that transaction, so use them outside one.
func (c *Conn) DB() *gbase.DB { return c.shared.db }

var (
	_ driver.ConnBeginTx                    = (*Conn)(nil)
	_ driver.ConnPrepareContext             = (*Conn)(nil)
	_ driver.ExecerContext                  = (*Conn)(nil)
	_ driver.QueryerContext                 = (*Conn)(nil)
	_ driver.NamedValueChecker              = (*Conn)(nil)
	_ driver.Pinger                         = (*Conn)(nil)
	_ driver.SessionResetter                = (*Conn)(nil)
	_ driver.Validator                      = (*Conn)(nil)
	_ driver.RowsColumnTypeDatabaseTypeName = (*rows)(nil)
	_ driver.RowsColumnTypeScanType         = (*rows)(nil)
)

func (c *Conn) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	if c.tx != nil {
		c.tx.Rollback()
		c.tx = nil
	}
	return c.shared.release()
}

func (c *Conn) Ping(ctx context.Context) error {
	if c.closed {
		return driver.ErrBadConn
	}
	return ctx.Err()
}

func (c *Conn) ResetSession(context.Context) error {
	if c.closed {
		return driver.ErrBadConn
	}
	return nil
}

func (c *Conn) IsValid() bool { return !c.closed }

func (c *Conn) Begin() (driver.Tx, error) { return c.BeginTx(context.Background(), driver.TxOptions{}) }

// BeginTx starts a serializable transaction; gbase runs one at a time, so
// every level up to serializable is met. ReadOnly rejects Exec in the transaction.
func (c *Conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.tx != nil {
		return nil, errors.New("gbase: transaction already active on this connection")
	}
	switch sql.IsolationLevel(opts.Isolation) {
	case sql.LevelDefault, sql.LevelReadUncommitted, sql.LevelReadCommitted, sql.LevelRepeatableRead, sql.LevelSnapshot, sql.LevelSerializable:
	default:
		return nil, fmt.Errorf("gbase: unsupported isolation level %v", sql.IsolationLevel(opts.Isolation))
	}
	tx, e := c.shared.db.Begin(ctx)
	if e != nil {
		return nil, e
	}
	c.tx, c.readOnly = tx, opts.ReadOnly
	return txn{c}, nil
}

type txn struct{ c *Conn }

func (t txn) Commit() error {
	tx := t.c.tx
	t.c.tx = nil
	if tx == nil {
		return gbase.ErrClosed
	}
	return tx.Commit()
}

func (t txn) Rollback() error {
	tx := t.c.tx
	t.c.tx = nil
	if tx == nil {
		return gbase.ErrClosed
	}
	return tx.Rollback()
}

func (c *Conn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

// PrepareContext checks the statements' syntax; gbase parses again on each run.
func (c *Conn) PrepareContext(_ context.Context, query string) (driver.Stmt, error) {
	segs, e := split(query)
	if e != nil {
		return nil, e
	}
	for _, s := range segs {
		if _, e = gsql.Parse(s.sql); e != nil {
			return nil, &gbase.Error{Code: "syntax", Message: e.Error()}
		}
	}
	return &stmt{c: c, segs: segs}, nil
}

func (c *Conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	segs, e := split(query)
	if e != nil {
		return nil, e
	}
	return c.exec(ctx, segs, args)
}

func (c *Conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	segs, e := split(query)
	if e != nil {
		return nil, e
	}
	return c.query(ctx, segs, args)
}

func (c *Conn) exec(ctx context.Context, segs []segment, args []driver.NamedValue) (driver.Result, error) {
	if c.closed {
		return nil, driver.ErrBadConn
	}
	if c.tx != nil && c.readOnly {
		return nil, errors.New("gbase: Exec in a read-only transaction")
	}
	values, e := bind(segs, args)
	if e != nil {
		return nil, e
	}
	if len(segs) == 0 {
		return result{}, nil
	}
	if c.tx == nil && len(segs) == 1 {
		r, e := c.shared.db.Exec(ctx, segs[0].sql, values[0]...)
		return newResult(r), e
	}
	tx, own := c.tx, false
	if tx == nil {
		if tx, e = c.shared.db.Begin(ctx); e != nil {
			return nil, e
		}
		own = true
		defer tx.Rollback()
	}
	var r gbase.Result
	for i, s := range segs {
		if r, e = tx.Exec(ctx, s.sql, values[i]...); e != nil {
			return nil, e
		}
	}
	if own {
		if e = tx.Commit(); e != nil {
			return nil, e
		}
	}
	return newResult(r), nil
}

func (c *Conn) query(ctx context.Context, segs []segment, args []driver.NamedValue) (driver.Rows, error) {
	if c.closed {
		return nil, driver.ErrBadConn
	}
	if len(segs) != 1 {
		return nil, errors.New("gbase: Query takes exactly one statement")
	}
	values, e := bind(segs, args)
	if e != nil {
		return nil, e
	}
	var r *gbase.Rows
	if c.tx != nil {
		r, e = c.tx.Query(ctx, segs[0].sql, values[0]...)
	} else {
		r, e = c.shared.db.Query(ctx, segs[0].sql, values[0]...)
	}
	if e != nil {
		return nil, e
	}
	defer r.Close()
	out := &rows{columns: r.Columns(), types: r.DeclTypes()}
	for r.Next() {
		out.data = append(out.data, r.Values())
	}
	if e = r.Err(); e != nil {
		return nil, e
	}
	return out, nil
}

// CheckNamedValue converts time.Time to the canonical TIMESTAMP text and bool
// to 0 or 1, and otherwise applies database/sql's default conversions.
func (c *Conn) CheckNamedValue(nv *driver.NamedValue) error {
	v, e := convert(nv.Value)
	if e != nil {
		return e
	}
	nv.Value = v
	return nil
}

func convert(v any) (driver.Value, error) {
	switch x := v.(type) {
	case nil, int64, float64, string, []byte:
		return x, nil
	case time.Time:
		return gbase.FormatTimestamp(x), nil
	case bool:
		if x {
			return int64(1), nil
		}
		return int64(0), nil
	}
	// Valuers may return bool or time.Time, so convert their result again.
	out, e := driver.DefaultParameterConverter.ConvertValue(v)
	if e != nil {
		return nil, e
	}
	switch out.(type) {
	case bool, time.Time:
		return convert(out)
	}
	return out, nil
}

type stmt struct {
	c    *Conn
	segs []segment
}

func (s *stmt) Close() error  { return nil }
func (s *stmt) NumInput() int { return -1 }

func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.c.exec(context.Background(), s.segs, named(args))
}

func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.c.query(context.Background(), s.segs, named(args))
}

func (s *stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.c.exec(ctx, s.segs, args)
}

func (s *stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.c.query(ctx, s.segs, args)
}

func named(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, v := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}

type result struct{ id, n int64 }

func newResult(r gbase.Result) result         { return result{r.LastInsertID, r.RowsAffected} }
func (r result) LastInsertId() (int64, error) { return r.id, nil }
func (r result) RowsAffected() (int64, error) { return r.n, nil }

type rows struct {
	columns, types []string
	data           [][]gbase.Value
	pos            int
}

func (r *rows) Columns() []string { return r.columns }
func (r *rows) Close() error      { r.data, r.pos = nil, 0; return nil }

func (r *rows) Next(dest []driver.Value) error {
	if r.pos >= len(r.data) {
		return io.EOF
	}
	row := r.data[r.pos]
	r.pos++
	for i, v := range row {
		dest[i] = scanValue(r.types[i], v)
	}
	return nil
}

func scanValue(declared string, v gbase.Value) driver.Value {
	switch declared {
	case "BOOLEAN":
		if n, ok := v.(int64); ok {
			return n != 0
		}
	case "DATE":
		if s, ok := v.(string); ok {
			if t, e := time.Parse(gbase.DateLayout, s); e == nil {
				return t
			}
		}
	case "DATETIME", "TIMESTAMP":
		if s, ok := v.(string); ok {
			if t, e := time.Parse(time.RFC3339Nano, s); e == nil {
				return t
			}
		}
	}
	return v
}

// ColumnTypeDatabaseTypeName is the declared type, or "" for an expression.
func (r *rows) ColumnTypeDatabaseTypeName(i int) string { return r.types[i] }

func (r *rows) ColumnTypeScanType(i int) reflect.Type {
	switch r.types[i] {
	case "INTEGER":
		return reflect.TypeFor[sql.NullInt64]()
	case "REAL":
		return reflect.TypeFor[sql.NullFloat64]()
	case "TEXT":
		return reflect.TypeFor[sql.NullString]()
	case "BLOB":
		return reflect.TypeFor[[]byte]()
	case "BOOLEAN":
		return reflect.TypeFor[sql.NullBool]()
	case "DATE", "DATETIME", "TIMESTAMP":
		return reflect.TypeFor[sql.NullTime]()
	}
	return reflect.TypeFor[any]()
}

// segment is one statement of a possibly multi-statement query. names[i] is
// the name of parameter i, or "" for ?.
type segment struct {
	sql   string
	names []string
}

// split cuts query at semicolons outside quotes and comments, using gbase's
// own lexer, and records each statement's parameters.
func split(query string) ([]segment, error) {
	tokens, e := gsql.Lex(query)
	if e != nil {
		return nil, &gbase.Error{Code: "syntax", Message: e.Error()}
	}
	var out []segment
	start, empty := 0, true
	for _, t := range tokens {
		end := t.Kind == gsql.TokenEOF || t.Kind == gsql.TokenSymbol && t.Text == ";"
		if !end {
			empty = false
			continue
		}
		if !empty {
			s, e := newSegment(query[start:t.Pos])
			if e != nil {
				return nil, e
			}
			out = append(out, s)
		}
		start, empty = t.Pos+1, true
	}
	return out, nil
}

func newSegment(text string) (segment, error) {
	tokens, e := gsql.Lex(text)
	if e != nil {
		return segment{}, &gbase.Error{Code: "syntax", Message: e.Error()}
	}
	s := segment{sql: strings.TrimSpace(text)}
	for _, t := range tokens {
		if t.Kind != gsql.TokenParameter {
			continue
		}
		i := t.Value.(int)
		if i == len(s.names) {
			name := ""
			if t.Text != "?" {
				name = t.Text[1:]
			}
			s.names = append(s.names, name)
		}
	}
	return s, nil
}

// bind assigns arguments to each segment's parameters: in order when they are
// positional, or by name when every argument is named.
func bind(segs []segment, args []driver.NamedValue) ([][]any, error) {
	byName := len(args) > 0 && args[0].Name != ""
	values := map[string]any{}
	for _, a := range args {
		if (a.Name != "") != byName {
			return nil, errors.New("gbase: cannot mix named and positional arguments")
		}
		if byName {
			if _, dup := values[a.Name]; dup {
				return nil, fmt.Errorf("gbase: duplicate argument %q", a.Name)
			}
			values[a.Name] = a.Value
		}
	}
	out := make([][]any, len(segs))
	pos := 0
	used := map[string]bool{}
	for i, s := range segs {
		out[i] = make([]any, len(s.names))
		for j, name := range s.names {
			if !byName {
				if pos == len(args) {
					return nil, fmt.Errorf("gbase: expected more than %d arguments", len(args))
				}
				out[i][j] = args[pos].Value
				pos++
				continue
			}
			if name == "" {
				return nil, errors.New("gbase: ? parameter with named arguments")
			}
			v, ok := values[name]
			if !ok {
				return nil, fmt.Errorf("gbase: missing argument %q", name)
			}
			out[i][j], used[name] = v, true
		}
	}
	if !byName && pos != len(args) {
		return nil, fmt.Errorf("gbase: expected %d arguments, got %d", pos, len(args))
	}
	if byName && len(used) != len(values) {
		for name := range values {
			if !used[name] {
				return nil, fmt.Errorf("gbase: unused argument %q", name)
			}
		}
	}
	return out, nil
}
