package gbase

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/rey4L/gbase/internal/storage"
)

func (db *DB) Tables(ctx context.Context) ([]string, error) {
	tx, e := db.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.done {
		return nil, ErrClosed
	}
	var out []string
	for _, t := range tx.cat.Tables {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out, nil
}
func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func literal(v Value) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'"
	case []byte:
		return fmt.Sprintf("X'%x'", x)
	default:
		return fmt.Sprint(x)
	}
}

func (db *DB) Schema(ctx context.Context) ([]string, error) {
	tx, e := db.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.done {
		return nil, ErrClosed
	}
	var out []string
	names := make([]string, 0, len(tx.cat.Tables))
	for n := range tx.cat.Tables {
		names = append(names, n)
	}
	sort.Strings(names)
	var ordered []string
	visiting := map[string]bool{}
	visited := map[string]bool{}
	var visit func(string) error
	visit = func(name string) error {
		if visited[name] {
			return nil
		}
		if visiting[name] {
			return fail("schema", "cyclic foreign key dependencies")
		}
		visiting[name] = true
		t := tx.cat.Tables[name]
		var parents []string
		for _, c := range t.Columns {
			if c.Ref != nil && canon(c.Ref.Table) != name {
				parents = append(parents, canon(c.Ref.Table))
			}
		}
		sort.Strings(parents)
		for _, p := range parents {
			if err := visit(p); err != nil {
				return err
			}
		}
		visiting[name] = false
		visited[name] = true
		ordered = append(ordered, name)
		return nil
	}
	for _, name := range names {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	for _, n := range ordered {
		t := tx.cat.Tables[n]
		var defs []string
		for _, c := range t.Columns {
			d := quoteIdent(c.Name) + " " + c.Type
			if c.Primary {
				d += " PRIMARY KEY"
			} else if c.Unique {
				d += " UNIQUE"
			}
			if c.NotNull && !c.Primary {
				d += " NOT NULL"
			}
			if c.Default != nil {
				v, e := decodeRecord(c.Default)
				if e != nil {
					return nil, e
				}
				d += " DEFAULT " + literal(v[0])
			}
			if c.Ref != nil {
				d += " REFERENCES " + quoteIdent(c.Ref.Table) + "(" + quoteIdent(c.Ref.Column) + ")"
			}
			defs = append(defs, d)
		}
		out = append(out, "CREATE TABLE "+quoteIdent(t.Name)+" ("+strings.Join(defs, ", ")+");")
		for _, i := range tx.indexes(t) {
			if i.Automatic {
				continue
			}
			unique := ""
			if i.Unique {
				unique = "UNIQUE "
			}
			out = append(out, "CREATE "+unique+"INDEX "+quoteIdent(i.Name)+" ON "+quoteIdent(i.Table)+"("+quoteIdent(i.Column)+");")
		}
	}
	return out, nil
}

func (tx *Tx) check() error {
	owners := map[uint32]string{}
	own := func(root uint32, name string) error {
		tr := storage.Tree{Tx: tx.pages, Root: root}
		pages, e := tr.Pages()
		if e != nil {
			return e
		}
		for _, p := range pages {
			if prev, ok := owners[p]; ok {
				return fail("corrupt", "page %d shared by %s and %s", p, prev, name)
			}
			owners[p] = name
		}
		return nil
	}
	if tx.pages.Root() != 0 {
		if e := own(tx.pages.Root(), "catalog"); e != nil {
			return e
		}
	}
	for name, t := range tx.cat.Tables {
		if name != canon(t.Name) || len(t.Columns) == 0 || t.NextID < 1 {
			return fail("corrupt", "invalid table metadata")
		}
		if e := own(t.Root, t.Name); e != nil {
			return e
		}
		rows, e := tx.scanTable(t)
		if e != nil {
			return e
		}
		for _, r := range rows {
			for p, c := range t.Columns {
				v, e := coerce(c, r.values[p])
				if e != nil {
					return fail("corrupt", "invalid column: %v", e)
				}
				if !sameValue(v, r.values[p]) {
					return fail("corrupt", "noncanonical stored type")
				}
				if c.Primary && c.Type == "INTEGER" && r.values[p] != r.id {
					return fail("corrupt", "primary key differs from row key")
				}
			}
			if e = tx.foreignKeys(t, r.values); e != nil {
				return fail("corrupt", "%v", e)
			}
		}
	}
	for name, i := range tx.cat.Indexes {
		if name != canon(i.Name) {
			return fail("corrupt", "invalid index name")
		}
		if e := own(i.Root, i.Name); e != nil {
			return e
		}
		t, e := tx.getTable(i.Table)
		if e != nil {
			return e
		}
		p := t.col(i.Column)
		if p < 0 {
			return fail("corrupt", "index column missing")
		}
		rows, e := tx.scanTable(t)
		if e != nil {
			return e
		}
		expected := map[string][]byte{}
		unique := map[string]bool{}
		for _, r := range rows {
			prefix, e := indexPrefix(r.values[p])
			if e != nil {
				return e
			}
			if i.Unique && r.values[p] != nil {
				if unique[string(prefix)] {
					return fail("corrupt", "duplicate unique index value")
				}
				unique[string(prefix)] = true
			}
			expected[string(append(prefix, rowKey(r.id)...))] = rowKey(r.id)
		}
		tr := storage.Tree{Tx: tx.pages, Root: i.Root}
		c, e := tr.Scan(nil, nil)
		if e != nil {
			return e
		}
		for c.Next() {
			key := string(c.Key())
			v, ok := expected[key]
			if !ok || !bytes.Equal(v, c.Value()) {
				return fail("corrupt", "unexpected index entry in %s", name)
			}
			delete(expected, key)
		}
		if e = c.Err(); e != nil {
			return e
		}
		if len(expected) != 0 {
			return fail("corrupt", "missing index entries")
		}
	}
	for _, p := range tx.pages.FreePages() {
		if prev, ok := owners[p]; ok {
			return fail("corrupt", "page %d both free and %s", p, prev)
		}
		owners[p] = "free"
	}
	for p := uint32(1); p < tx.pages.PageCount(); p++ {
		if _, ok := owners[p]; !ok {
			return fail("corrupt", "orphan page %d", p)
		}
	}
	return nil
}
