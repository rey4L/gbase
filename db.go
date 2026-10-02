package gbase

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/rey4L/gbase/internal/sql"
	"github.com/rey4L/gbase/internal/storage"
)

type DB struct {
	pager  *storage.Pager
	gate   chan struct{}
	mu     sync.Mutex
	closed bool
	active *Tx
}
type Tx struct {
	db     *DB
	pages  *storage.Tx
	cat    *catalog
	ctx    context.Context
	mu     sync.Mutex
	done   bool
	cursor *Rows
}
type Result struct {
	RowsAffected int64
	LastInsertID int64
}
type Rows struct {
	tx      *Tx
	ctx     context.Context
	next    func() ([]Value, bool, error)
	cleanup func()

	columns []string
	rows    [][]Value
	pos     int
	current []Value
	err     error
	closed  bool
	release func()
	mu      sync.Mutex
}

func Open(path string) (*DB, error) {
	p, e := storage.Open(path)
	if e != nil {
		return nil, e
	}
	d := &DB{pager: p, gate: make(chan struct{}, 1)}
	d.gate <- struct{}{}
	return d, nil
}

func (db *DB) Begin(ctx context.Context) (*Tx, error) {
	if ctx == nil {
		return nil, fail("context", "nil context")
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-db.gate:
	}
	if e := ctx.Err(); e != nil {
		db.gate <- struct{}{}
		return nil, e
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		db.gate <- struct{}{}
		return nil, ErrClosed
	}
	p, e := db.pager.Begin()
	if e != nil {
		db.gate <- struct{}{}
		return nil, e
	}
	tx := &Tx{db: db, pages: p, ctx: ctx}
	if e = tx.loadCatalog(); e != nil {
		p.Rollback()
		db.gate <- struct{}{}
		return nil, e
	}
	db.active = tx
	return tx, nil
}

func (db *DB) Close() error {
	db.mu.Lock()
	if db.closed {
		db.mu.Unlock()
		return nil
	}
	db.closed = true
	tx := db.active
	db.mu.Unlock()
	if tx != nil {
		tx.Rollback()
	}
	<-db.gate
	e := db.pager.Close()
	db.gate <- struct{}{}
	return e
}

func (db *DB) Exec(ctx context.Context, query string, args ...any) (Result, error) {
	tx, e := db.Begin(ctx)
	if e != nil {
		return Result{}, e
	}
	defer tx.Rollback()
	r, e := tx.Exec(ctx, query, args...)
	if e != nil {
		return Result{}, e
	}
	e = tx.Commit()
	return r, e
}

func (db *DB) Query(ctx context.Context, query string, args ...any) (*Rows, error) {
	tx, e := db.Begin(ctx)
	if e != nil {
		return nil, e
	}
	rows, e := tx.Query(ctx, query, args...)
	if e != nil {
		tx.Rollback()
		return nil, e
	}
	tx.mu.Lock()
	if tx.done {
		tx.mu.Unlock()
		rows.Close()
		return nil, ErrClosed
	}
	rows.mu.Lock()
	rows.release = func() {
		tx.mu.Lock()
		if tx.cursor == rows {
			tx.cursor = nil
		}
		tx.mu.Unlock()
		tx.Rollback()
	}
	rows.mu.Unlock()
	tx.mu.Unlock()
	return rows, nil
}

func parameters(args []any) ([]Value, error) {
	out := make([]Value, len(args))
	for i, v := range args {
		var e error
		out[i], e = normalize(v)
		if e != nil {
			return nil, e
		}
	}
	return out, nil
}

func (tx *Tx) ready(ctx context.Context) error {
	if tx.done {
		return ErrClosed
	}
	if tx.cursor != nil {
		return ErrBusy
	}
	if ctx == nil {
		return fail("context", "nil context")
	}
	if e := tx.ctx.Err(); e != nil {
		return e
	}
	return ctx.Err()
}

func (tx *Tx) Exec(ctx context.Context, query string, args ...any) (Result, error) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if e := tx.ready(ctx); e != nil {
		return Result{}, e
	}
	stmt, e := sql.Parse(query)
	if e != nil {
		return Result{}, fail("syntax", "%v", e)
	}
	params, e := parameters(args)
	if e != nil {
		return Result{}, e
	}
	if e = checkParameters(stmt, len(params)); e != nil {
		return Result{}, e
	}
	snap := tx.pages.Savepoint()
	cat := cloneCatalog(tx.cat)
	oldctx := tx.ctx
	operation, cleanup := operationContext(oldctx, ctx)
	tx.ctx = operation
	defer func() { tx.ctx = oldctx; cleanup() }()
	r, e := tx.execute(stmt, params)
	if e == nil {
		e = tx.ctx.Err()
	}
	if e == nil {
		e = tx.validateCatalog()
	}
	if e == nil {
		e = tx.saveCatalog()
	}
	if e != nil {
		tx.pages.Restore(snap)
		tx.cat = cat
		return Result{}, e
	}
	return r, nil
}

func (tx *Tx) Query(ctx context.Context, query string, args ...any) (*Rows, error) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if e := tx.ready(ctx); e != nil {
		return nil, e
	}
	stmt, e := sql.Parse(query)
	if e != nil {
		return nil, fail("syntax", "%v", e)
	}
	params, e := parameters(args)
	if e != nil {
		return nil, e
	}
	if e = checkParameters(stmt, len(params)); e != nil {
		return nil, e
	}
	oldctx := tx.ctx
	operation, cleanup := operationContext(oldctx, ctx)
	tx.ctx = operation
	owned := false
	defer func() {
		tx.ctx = oldctx
		if !owned {
			cleanup()
		}
	}()
	var q *queryResult
	switch s := stmt.(type) {
	case *sql.Select:
		q, e = tx.executeSelect(s, params)
	case *sql.Explain:
		q, e = tx.explain(s.Statement, params)
	default:
		return nil, fail("statement", "Query requires SELECT or EXPLAIN")
	}
	if e != nil {
		return nil, e
	}
	rows := &Rows{tx: tx, ctx: operation, cleanup: cleanup, next: q.next, columns: q.columns, rows: q.rows, pos: -1}
	owned = true
	rows.release = func() { tx.mu.Lock(); tx.cursor = nil; tx.mu.Unlock() }
	tx.cursor = rows
	return rows, nil
}

func (tx *Tx) finish(commit bool) error {
	tx.mu.Lock()
	if tx.done {
		tx.mu.Unlock()
		return ErrClosed
	}
	if commit && tx.cursor != nil {
		tx.mu.Unlock()
		return ErrBusy
	}
	if commit {
		if e := tx.ctx.Err(); e != nil {
			tx.mu.Unlock()
			return e
		}
	}
	rows := tx.cursor
	tx.cursor = nil
	tx.done = true
	var e error
	if commit {
		e = tx.pages.Commit()
	} else {
		e = tx.pages.Rollback()
	}
	tx.mu.Unlock()
	if rows != nil {
		rows.mu.Lock()
		rows.closed = true
		if rows.err == nil {
			rows.err = ErrClosed
		}
		rows.release = nil
		rows.next = nil
		rows.rows = nil
		rows.current = nil
		cleanup := rows.cleanup
		rows.cleanup = nil
		rows.mu.Unlock()
		if cleanup != nil {
			cleanup()
		}
	}
	tx.db.mu.Lock()
	if tx.db.active == tx {
		tx.db.active = nil
	}
	tx.db.mu.Unlock()
	tx.db.gate <- struct{}{}
	return e
}
func (tx *Tx) Commit() error { return tx.finish(true) }
func (tx *Tx) Rollback() error {
	e := tx.finish(false)
	if errors.Is(e, ErrClosed) {
		return nil
	}
	return e
}
func (r *Rows) Columns() []string { return append([]string(nil), r.columns...) }
func (r *Rows) Next() bool {
	r.tx.mu.Lock()
	r.mu.Lock()
	if r.closed || r.tx.done {
		if r.tx.done && !r.closed && r.err == nil {
			r.err = ErrClosed
		}
		r.mu.Unlock()
		r.tx.mu.Unlock()
		return false
	}
	var ok bool
	if err := r.ctx.Err(); err != nil {
		r.err = err
	} else if r.next != nil {
		r.current, ok, r.err = r.next()
	} else {
		r.pos++
		ok = r.pos < len(r.rows)
		if ok {
			r.current = r.rows[r.pos]
		}
	}
	r.mu.Unlock()
	r.tx.mu.Unlock()
	if !ok {
		r.Close()
	}
	return ok
}

func (r *Rows) Values() []Value {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]Value(nil), r.current...)
	for i, v := range out {
		if b, ok := v.([]byte); ok {
			out[i] = append([]byte(nil), b...)
		}
	}
	return out
}
func (r *Rows) Err() error { r.mu.Lock(); defer r.mu.Unlock(); return r.err }
func (r *Rows) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	release := r.release
	cleanup := r.cleanup
	r.release = nil
	r.cleanup = nil
	r.next = nil
	r.rows = nil
	r.current = nil
	r.mu.Unlock()
	if cleanup != nil {
		cleanup()
	}
	if release != nil {
		release()
	}
	return nil
}

func (db *DB) Check(ctx context.Context) error {
	tx, e := db.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.done {
		return ErrClosed
	}
	return tx.check()
}

func (tx *Tx) explain(stmt sql.Statement, args []Value) (*queryResult, error) {
	s, ok := stmt.(*sql.Select)
	if !ok {
		return nil, fmt.Errorf("EXPLAIN currently supports SELECT")
	}
	return tx.explainSelect(s, args)
}

func operationContext(parent, operation context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(operation)
	stop := context.AfterFunc(parent, cancel)
	return ctx, func() { stop(); cancel() }
}
