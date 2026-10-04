package gbase

import (
	"github.com/rey4L/gbase/internal/sql"
	"github.com/rey4L/gbase/internal/storage"
)

// alterTable rewrites every row when the column set changes, so stored records
// always match the schema width and no on-disk format change is needed. Exec's
// statement savepoint undoes partial work on any error.
func (tx *Tx) alterTable(s *sql.AlterTable) error {
	t, e := tx.getTable(s.Table)
	if e != nil {
		return e
	}
	switch s.Action {
	case "ADD COLUMN":
		return tx.addColumn(t, s.Column)
	case "DROP COLUMN":
		return tx.dropColumn(t, s.From)
	case "RENAME TO":
		return tx.renameTable(t, s.To)
	case "RENAME COLUMN":
		return tx.renameColumn(t, s.From, s.To)
	}
	return fail("statement", "unsupported ALTER TABLE action %s", s.Action)
}

func (tx *Tx) rewriteRows(t *table, change func([]Value) []Value) error {
	rows, e := tx.scanTable(t)
	if e != nil {
		return e
	}
	tr := storage.Tree{Tx: tx.pages, Root: t.Root}
	for _, r := range rows {
		data, e := encodeRecord(change(r.values))
		if e != nil {
			return e
		}
		if e = tr.Put(rowKey(r.id), data); e != nil {
			return e
		}
	}
	t.Root = tr.Root
	return nil
}

func (tx *Tx) addColumn(t *table, d sql.ColumnDef) error {
	if t.col(d.Name) >= 0 {
		return fail("schema", "column %s exists", d.Name)
	}
	if d.PrimaryKey || d.Unique {
		return fail("schema", "ADD COLUMN cannot add PRIMARY KEY or UNIQUE; add the column, then CREATE UNIQUE INDEX")
	}
	c, e := columnFromDef(d)
	if e != nil {
		return e
	}
	var v Value
	if c.Default != nil {
		values, e := decodeRecord(c.Default)
		if e != nil {
			return e
		}
		v = values[0]
	}
	if c.NotNull && v == nil {
		return fail("constraint", "NOT NULL column %s needs a non-NULL DEFAULT", d.Name)
	}
	if c.Ref != nil && v != nil {
		return fail("constraint", "REFERENCES column %s needs a NULL DEFAULT", d.Name)
	}
	if e = tx.checkReference(c); e != nil {
		return e
	}
	if e = tx.rewriteRows(t, func(values []Value) []Value { return append(values, v) }); e != nil {
		return e
	}
	t.Columns = append(t.Columns, c)
	return nil
}

func (tx *Tx) dropColumn(t *table, name string) error {
	p := t.col(name)
	if p < 0 {
		return fail("schema", "unknown column %s", name)
	}
	c := t.Columns[p]
	if len(t.Columns) == 1 {
		return fail("schema", "cannot drop the only column of %s", t.Name)
	}
	if c.Primary {
		return fail("schema", "cannot drop primary key column %s", c.Name)
	}
	for _, i := range tx.indexes(t) {
		if i.covers(c.Name) {
			if i.Automatic {
				return fail("schema", "cannot drop UNIQUE column %s", c.Name)
			}
			return fail("schema", "column %s is used by index %s", c.Name, i.Name)
		}
	}
	if child := tx.referencing(t, c.Name); child != "" {
		return fail("constraint", "column %s referenced by %s", c.Name, child)
	}
	if e := tx.rewriteRows(t, func(values []Value) []Value { return append(values[:p:p], values[p+1:]...) }); e != nil {
		return e
	}
	t.Columns = append(t.Columns[:p:p], t.Columns[p+1:]...)
	return nil
}

// referencing names a table with a foreign key to t.column, or "" when none does.
func (tx *Tx) referencing(t *table, column string) string {
	for _, child := range tx.cat.Tables {
		for _, c := range child.Columns {
			if c.Ref != nil && canon(c.Ref.Table) == canon(t.Name) && canon(c.Ref.Column) == canon(column) {
				return child.Name
			}
		}
	}
	return ""
}

func (tx *Tx) renameTable(t *table, to string) error {
	old := canon(t.Name)
	if other := tx.cat.Tables[canon(to)]; other != nil && other != t {
		return fail("schema", "table %s exists", to)
	}
	if tx.cat.Indexes[canon(to)] != nil {
		return fail("schema", "name %s already used", to)
	}
	indexes := tx.indexes(t)
	delete(tx.cat.Tables, old)
	t.Name = to
	tx.cat.Tables[canon(to)] = t
	for _, i := range indexes {
		i.Table = to
	}
	for _, child := range tx.cat.Tables {
		for _, c := range child.Columns {
			if c.Ref != nil && canon(c.Ref.Table) == old {
				c.Ref.Table = to
			}
		}
	}
	return tx.renameAutomaticIndexes(t)
}

func (tx *Tx) renameColumn(t *table, from, to string) error {
	p := t.col(from)
	if p < 0 {
		return fail("schema", "unknown column %s", from)
	}
	if q := t.col(to); q >= 0 && q != p {
		return fail("schema", "column %s exists", to)
	}
	old := canon(t.Columns[p].Name)
	t.Columns[p].Name = to
	for _, i := range tx.indexes(t) {
		if canon(i.Column) == old {
			i.Column = to
		}
		for k, c := range i.Columns {
			if canon(c) == old {
				i.Columns[k] = to
			}
		}
	}
	for _, child := range tx.cat.Tables {
		for _, c := range child.Columns {
			if c.Ref != nil && canon(c.Ref.Table) == canon(t.Name) && canon(c.Ref.Column) == old {
				c.Ref.Column = to
			}
		}
	}
	return tx.renameAutomaticIndexes(t)
}

// renameAutomaticIndexes re-keys t's UNIQUE constraint indexes to the names
// createTable derives, so a later table reusing an old name cannot collide.
func (tx *Tx) renameAutomaticIndexes(t *table) error {
	var moved []*index
	for _, i := range tx.indexes(t) {
		if i.Automatic {
			delete(tx.cat.Indexes, canon(i.Name))
			moved = append(moved, i)
		}
	}
	for _, i := range moved {
		i.Name = automaticIndexName(t, i.cols())
		if tx.cat.Indexes[canon(i.Name)] != nil || tx.cat.Tables[canon(i.Name)] != nil {
			return fail("schema", "reserved index name collision")
		}
		tx.cat.Indexes[canon(i.Name)] = i
	}
	return nil
}
